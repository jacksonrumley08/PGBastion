package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacksonrumley08/pgbastion/internal/consensus"
)

// ParameterContext indicates when a parameter change takes effect.
type ParameterContext string

const (
	// ContextPostmaster requires a PostgreSQL restart.
	ContextPostmaster ParameterContext = "postmaster"
	// ContextSighup requires a reload (pg_reload_conf).
	ContextSighup ParameterContext = "sighup"
	// ContextSuperuser can be changed at runtime by superuser.
	ContextSuperuser ParameterContext = "superuser"
	// ContextUser can be changed at runtime by any user.
	ContextUser ParameterContext = "user"
	// ContextBackend applies to new connections only.
	ContextBackend ParameterContext = "backend"
	// ContextSuperuserBackend applies to new superuser connections.
	ContextSuperuserBackend ParameterContext = "superuser-backend"
)

// ParameterInfo holds information about a PostgreSQL parameter.
type ParameterInfo struct {
	Name           string           `json:"name"`
	Setting        string           `json:"setting"`
	Unit           string           `json:"unit,omitempty"`
	Context        ParameterContext `json:"context"`
	PendingRestart bool             `json:"pending_restart"`
}

// ConfigApplier applies PostgreSQL configuration from the Raft FSM.
type ConfigApplier struct {
	connector PGConnector
	store     *consensus.Store
	logger    *slog.Logger

	mu                  sync.RWMutex
	lastAppliedVersion  int64
	pendingRestart      bool
	pendingRestartParams []string
}

// NewConfigApplier creates a new ConfigApplier.
func NewConfigApplier(connector PGConnector, store *consensus.Store, logger *slog.Logger) *ConfigApplier {
	return &ConfigApplier{
		connector: connector,
		store:     store,
		logger:    logger.With("component", "config-applier"),
	}
}

// IsPendingRestart returns true if PostgreSQL needs a restart for config changes.
func (a *ConfigApplier) IsPendingRestart() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.pendingRestart
}

// PendingRestartParams returns parameters that require a restart.
func (a *ConfigApplier) PendingRestartParams() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]string(nil), a.pendingRestartParams...)
}

// LastAppliedVersion returns the last applied config version.
func (a *ConfigApplier) LastAppliedVersion() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastAppliedVersion
}

// ApplyConfig checks for config updates and applies them via ALTER SYSTEM.
func (a *ConfigApplier) ApplyConfig(ctx context.Context, dsn string) error {
	if a.store == nil {
		return nil
	}

	cfg := a.store.GetPostgreSQLConfig()

	a.mu.RLock()
	lastVersion := a.lastAppliedVersion
	a.mu.RUnlock()

	// No changes since last apply.
	if cfg.Version <= lastVersion {
		return nil
	}

	if len(cfg.Parameters) == 0 {
		a.mu.Lock()
		a.lastAppliedVersion = cfg.Version
		a.mu.Unlock()
		return nil
	}

	conn, err := a.connector.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	defer conn.Close(ctx)

	// Get current parameter contexts for restart detection.
	paramContexts, err := a.getParameterContexts(ctx, conn)
	if err != nil {
		a.logger.Warn("failed to get parameter contexts, assuming all need restart", "error", err)
	}

	var restartRequired []string

	// Apply each parameter via ALTER SYSTEM.
	for name, value := range cfg.Parameters {
		if err := a.applyParameter(ctx, conn, name, value); err != nil {
			a.logger.Error("failed to apply parameter", "name", name, "error", err)
			continue
		}

		// Check if this parameter requires restart.
		if info, ok := paramContexts[name]; ok {
			if info.Context == ContextPostmaster {
				restartRequired = append(restartRequired, name)
			}
		}
	}

	// Reload configuration for non-restart parameters.
	if err := a.reloadConfig(ctx, conn); err != nil {
		a.logger.Warn("failed to reload config", "error", err)
	}

	a.mu.Lock()
	a.lastAppliedVersion = cfg.Version
	if len(restartRequired) > 0 {
		a.pendingRestart = true
		a.pendingRestartParams = restartRequired
	}
	a.mu.Unlock()

	a.logger.Info("applied PostgreSQL config",
		"version", cfg.Version,
		"parameters", len(cfg.Parameters),
		"restart_required", len(restartRequired) > 0,
	)

	return nil
}

// applyParameter applies a single parameter via ALTER SYSTEM SET.
func (a *ConfigApplier) applyParameter(ctx context.Context, conn PGConn, name, value string) error {
	// Validate parameter name (prevent SQL injection).
	if !isValidParameterName(name) {
		return fmt.Errorf("invalid parameter name: %s", name)
	}

	// Use parameterized query for the value.
	// Note: ALTER SYSTEM doesn't support parameterized queries, so we need to quote the value.
	quotedValue := quoteParameterValue(value)
	sql := fmt.Sprintf("ALTER SYSTEM SET %s = %s", name, quotedValue)

	_, err := conn.Query(ctx, sql)
	if err != nil {
		return fmt.Errorf("ALTER SYSTEM SET %s: %w", name, err)
	}

	a.logger.Debug("applied parameter", "name", name, "value", value)
	return nil
}

// reloadConfig sends pg_reload_conf() to apply non-restart parameters.
func (a *ConfigApplier) reloadConfig(ctx context.Context, conn PGConn) error {
	_, err := conn.Query(ctx, "SELECT pg_reload_conf()")
	return err
}

// getParameterContexts retrieves context information for all parameters.
func (a *ConfigApplier) getParameterContexts(ctx context.Context, conn PGConn) (map[string]ParameterInfo, error) {
	rows, err := conn.Query(ctx, `
		SELECT name, setting, unit, context, pending_restart
		FROM pg_settings
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]ParameterInfo)
	for rows.Next() {
		var info ParameterInfo
		var unit *string
		if err := rows.Scan(&info.Name, &info.Setting, &unit, &info.Context, &info.PendingRestart); err != nil {
			continue
		}
		if unit != nil {
			info.Unit = *unit
		}
		result[info.Name] = info
	}
	return result, rows.Err()
}

// GetCurrentSettings retrieves current PostgreSQL settings.
func (a *ConfigApplier) GetCurrentSettings(ctx context.Context, dsn string, names []string) (map[string]ParameterInfo, error) {
	conn, err := a.connector.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	defer conn.Close(ctx)

	if len(names) == 0 {
		return a.getParameterContexts(ctx, conn)
	}

	// Query only specified parameters.
	result := make(map[string]ParameterInfo)
	for _, name := range names {
		if !isValidParameterName(name) {
			continue
		}
		row := conn.QueryRow(ctx, `
			SELECT name, setting, unit, context, pending_restart
			FROM pg_settings
			WHERE name = $1
		`, name)
		var info ParameterInfo
		var unit *string
		if err := row.Scan(&info.Name, &info.Setting, &unit, &info.Context, &info.PendingRestart); err != nil {
			continue
		}
		if unit != nil {
			info.Unit = *unit
		}
		result[info.Name] = info
	}
	return result, nil
}

// ValidateParameters checks if parameters are valid PostgreSQL settings.
func (a *ConfigApplier) ValidateParameters(ctx context.Context, dsn string, params map[string]string) ([]string, error) {
	conn, err := a.connector.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	defer conn.Close(ctx)

	var invalid []string
	for name := range params {
		if !isValidParameterName(name) {
			invalid = append(invalid, name)
			continue
		}

		// Check if parameter exists in pg_settings.
		var exists bool
		row := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_settings WHERE name = $1)", name)
		if err := row.Scan(&exists); err != nil || !exists {
			invalid = append(invalid, name)
		}
	}

	sort.Strings(invalid)
	return invalid, nil
}

// ClearPendingRestart clears the pending restart flag (call after restart).
func (a *ConfigApplier) ClearPendingRestart() {
	a.mu.Lock()
	a.pendingRestart = false
	a.pendingRestartParams = nil
	a.mu.Unlock()
}

// ResetParameter removes a parameter from postgresql.auto.conf.
func (a *ConfigApplier) ResetParameter(ctx context.Context, dsn string, name string) error {
	if !isValidParameterName(name) {
		return fmt.Errorf("invalid parameter name: %s", name)
	}

	conn, err := a.connector.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	defer conn.Close(ctx)

	sql := fmt.Sprintf("ALTER SYSTEM RESET %s", name)
	_, err = conn.Query(ctx, sql)
	if err != nil {
		return fmt.Errorf("ALTER SYSTEM RESET %s: %w", name, err)
	}

	return a.reloadConfig(ctx, conn)
}

// isValidParameterName checks if a parameter name is safe (no SQL injection).
func isValidParameterName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// quoteParameterValue quotes a parameter value for ALTER SYSTEM.
func quoteParameterValue(value string) string {
	// Simple quoting: wrap in single quotes and escape single quotes.
	escaped := strings.ReplaceAll(value, "'", "''")
	return "'" + escaped + "'"
}

// ConfigWatcher watches for PostgreSQL config changes and applies them.
type ConfigWatcher struct {
	applier  *ConfigApplier
	dsn      string
	interval time.Duration
	logger   *slog.Logger
}

// NewConfigWatcher creates a new ConfigWatcher.
func NewConfigWatcher(applier *ConfigApplier, dsn string, interval time.Duration, logger *slog.Logger) *ConfigWatcher {
	return &ConfigWatcher{
		applier:  applier,
		dsn:      dsn,
		interval: interval,
		logger:   logger.With("component", "config-watcher"),
	}
}

// Run starts watching for config changes. Blocks until context is cancelled.
func (w *ConfigWatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Initial apply.
	if err := w.applier.ApplyConfig(ctx, w.dsn); err != nil {
		w.logger.Warn("initial config apply failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.applier.ApplyConfig(ctx, w.dsn); err != nil {
				w.logger.Warn("config apply failed", "error", err)
			}
		}
	}
}
