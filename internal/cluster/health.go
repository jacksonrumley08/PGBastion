package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	replicationLagBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_replication_lag_bytes",
		Help: "Replication lag in bytes (replica only)",
	})

	healthCheckDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pgbastion_health_check_duration_seconds",
		Help:    "Duration of health check operations in seconds",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 8), // 10ms to 2.5s
	})

	healthCheckTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_health_check_total",
		Help: "Total number of health checks",
	}, []string{"result"})
)

// HealthStatus represents the result of a health check cycle.
type HealthStatus struct {
	Healthy          bool      `json:"healthy"`
	IsInRecovery     bool      `json:"is_in_recovery"`
	AcceptingConns   bool      `json:"accepting_connections"`
	ReplicationLag   int64     `json:"replication_lag_bytes"`
	ReplicationSlots []SlotInfo `json:"replication_slots,omitempty"`
	Replicas         []ReplicaInfo `json:"replicas,omitempty"`
	ActiveConns      int       `json:"active_connections"`
	LastCheck        time.Time `json:"last_check"`
	Error            string    `json:"error,omitempty"`
}

// SlotInfo represents a replication slot.
type SlotInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Active   bool   `json:"active"`
	WALBytes int64  `json:"wal_bytes"`
}

// ReplicaInfo represents a streaming replica as seen from the primary.
type ReplicaInfo struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	SentLag    int64  `json:"sent_lag_bytes"`
	WriteLag   int64  `json:"write_lag_bytes"`
	FlushLag   int64  `json:"flush_lag_bytes"`
	ReplayLag  int64  `json:"replay_lag_bytes"`
	SyncState  string `json:"sync_state"`
}

// PGConnector abstracts PostgreSQL connectivity for testing.
type PGConnector interface {
	Connect(ctx context.Context, dsn string) (PGConn, error)
}

// PGConn abstracts a PostgreSQL connection for testing.
type PGConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Close(ctx context.Context) error
}

// pgxConnector is the real PGConnector using pgx.
type pgxConnector struct{}

func (c *pgxConnector) Connect(ctx context.Context, dsn string) (PGConn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &pgxConn{conn: conn}, nil
}

type pgxConn struct {
	conn *pgx.Conn
}

func (c *pgxConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return c.conn.QueryRow(ctx, sql, args...)
}

func (c *pgxConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.conn.Query(ctx, sql, args...)
}

func (c *pgxConn) Close(ctx context.Context) error {
	return c.conn.Close(ctx)
}

// DefaultHealthTimeout is the default timeout for health check connections.
const DefaultHealthTimeout = 5 * time.Second

// DefaultHistorySize is the default number of health check results to retain.
const DefaultHistorySize = 100

// HealthCheckEntry represents a single health check result with timestamp.
type HealthCheckEntry struct {
	Status    HealthStatus `json:"status"`
	Timestamp time.Time    `json:"timestamp"`
	Duration  time.Duration `json:"duration_ms"`
}

// HealthHistory is a thread-safe ring buffer for health check history.
type HealthHistory struct {
	mu      sync.RWMutex
	entries []HealthCheckEntry
	head    int
	size    int
	cap     int
}

// NewHealthHistory creates a new ring buffer with the given capacity.
func NewHealthHistory(capacity int) *HealthHistory {
	return &HealthHistory{
		entries: make([]HealthCheckEntry, capacity),
		cap:     capacity,
	}
}

// Add adds a new entry to the ring buffer.
func (h *HealthHistory) Add(entry HealthCheckEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries[h.head] = entry
	h.head = (h.head + 1) % h.cap
	if h.size < h.cap {
		h.size++
	}
}

// GetAll returns all entries in chronological order (oldest first).
func (h *HealthHistory) GetAll() []HealthCheckEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]HealthCheckEntry, h.size)
	if h.size == 0 {
		return result
	}

	start := 0
	if h.size == h.cap {
		start = h.head // oldest entry is at head when full
	}

	for i := 0; i < h.size; i++ {
		idx := (start + i) % h.cap
		result[i] = h.entries[idx]
	}
	return result
}

// GetRecent returns the N most recent entries (newest first).
func (h *HealthHistory) GetRecent(n int) []HealthCheckEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if n > h.size {
		n = h.size
	}
	if n == 0 {
		return nil
	}

	result := make([]HealthCheckEntry, n)
	for i := 0; i < n; i++ {
		// head-1 is the most recent, head-2 is second most recent, etc.
		idx := (h.head - 1 - i + h.cap) % h.cap
		result[i] = h.entries[idx]
	}
	return result
}

// Stats returns summary statistics about the health history.
func (h *HealthHistory) Stats() map[string]any {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.size == 0 {
		return map[string]any{
			"total_checks":     0,
			"success_count":    0,
			"failure_count":    0,
			"success_rate":     0.0,
			"avg_duration_ms":  0.0,
		}
	}

	var successCount, failureCount int
	var totalDuration time.Duration

	for i := 0; i < h.size; i++ {
		entry := h.entries[i]
		if entry.Status.Healthy {
			successCount++
		} else {
			failureCount++
		}
		totalDuration += entry.Duration
	}

	return map[string]any{
		"total_checks":     h.size,
		"success_count":    successCount,
		"failure_count":    failureCount,
		"success_rate":     float64(successCount) / float64(h.size) * 100,
		"avg_duration_ms":  float64(totalDuration.Milliseconds()) / float64(h.size),
	}
}

// HealthChecker periodically checks local PostgreSQL health.
type HealthChecker struct {
	dsn               string
	interval          time.Duration
	maxBackoff        time.Duration
	backoffMultiplier float64
	healthTimeout     time.Duration
	connector         PGConnector
	logger            *slog.Logger

	mu        sync.RWMutex
	status    HealthStatus
	history   *HealthHistory
	readyCh   chan struct{}
	readyOnce sync.Once

	// Backoff state.
	consecutiveFailures int
	currentInterval     time.Duration

	// connMu protects the persistent connection.
	connMu  sync.Mutex
	conn    PGConn
	connDSN string
}

// NewHealthChecker creates a new HealthChecker.
func NewHealthChecker(dsn string, interval, maxBackoff time.Duration, backoffMultiplier float64, healthTimeout time.Duration, logger *slog.Logger) *HealthChecker {
	if healthTimeout == 0 {
		healthTimeout = DefaultHealthTimeout
	}
	if maxBackoff == 0 {
		maxBackoff = 30 * time.Second
	}
	if backoffMultiplier == 0 {
		backoffMultiplier = 2.0
	}
	return &HealthChecker{
		dsn:               dsn,
		interval:          interval,
		maxBackoff:        maxBackoff,
		backoffMultiplier: backoffMultiplier,
		healthTimeout:     healthTimeout,
		connector:         &pgxConnector{},
		logger:            logger.With("component", "health-checker"),
		history:           NewHealthHistory(DefaultHistorySize),
		readyCh:           make(chan struct{}),
		currentInterval:   interval,
	}
}

// NewHealthCheckerWithConnector creates a HealthChecker with a custom connector (for testing).
func NewHealthCheckerWithConnector(dsn string, interval time.Duration, connector PGConnector, logger *slog.Logger) *HealthChecker {
	return &HealthChecker{
		dsn:               dsn,
		interval:          interval,
		maxBackoff:        30 * time.Second,
		backoffMultiplier: 2.0,
		healthTimeout:     DefaultHealthTimeout,
		connector:         connector,
		logger:            logger.With("component", "health-checker"),
		history:           NewHealthHistory(DefaultHistorySize),
		readyCh:           make(chan struct{}),
		currentInterval:   interval,
	}
}

// History returns the health check history.
func (h *HealthChecker) History() *HealthHistory {
	return h.history
}

// Ready returns a channel that is closed after the first health check completes.
func (h *HealthChecker) Ready() <-chan struct{} {
	return h.readyCh
}

// Status returns a copy of the latest health status.
func (h *HealthChecker) Status() HealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.status
}

// Run starts the health check loop. Blocks until context is cancelled.
func (h *HealthChecker) Run(ctx context.Context) error {
	h.logger.Info("starting health checker",
		"interval", h.interval,
		"max_backoff", h.maxBackoff,
		"backoff_multiplier", h.backoffMultiplier,
		"dsn", h.dsn,
	)

	// Initial check.
	h.check(ctx)

	for {
		// Use current interval (may be backed off).
		h.mu.RLock()
		interval := h.currentInterval
		h.mu.RUnlock()

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			h.logger.Info("health checker shutting down")
			h.closeConn(ctx)
			return ctx.Err()
		case <-timer.C:
			h.check(ctx)
		}
	}
}

// getConn returns a reusable connection, creating one if necessary.
// The connection is tested with a simple query before being returned.
func (h *HealthChecker) getConn(ctx context.Context) (PGConn, error) {
	h.connMu.Lock()
	defer h.connMu.Unlock()

	// If we have a connection, test it's still alive.
	if h.conn != nil {
		// Quick ping test - if it fails, close and reconnect.
		var one int
		if err := h.conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			h.logger.Debug("existing connection failed ping, reconnecting", "error", err)
			h.conn.Close(ctx)
			h.conn = nil
		} else {
			return h.conn, nil
		}
	}

	// Create new connection.
	conn, err := h.connector.Connect(ctx, h.dsn)
	if err != nil {
		return nil, err
	}
	h.conn = conn
	h.connDSN = h.dsn
	h.logger.Debug("established new health check connection")
	return conn, nil
}

// closeConn closes the persistent connection if open.
func (h *HealthChecker) closeConn(ctx context.Context) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.conn != nil {
		h.conn.Close(ctx)
		h.conn = nil
	}
}

func (h *HealthChecker) check(ctx context.Context) {
	// Respect context cancellation before starting work.
	if ctx.Err() != nil {
		return
	}

	startTime := time.Now()
	checkCtx, cancel := context.WithTimeout(ctx, h.healthTimeout)
	defer cancel()

	status := HealthStatus{
		LastCheck: startTime,
	}

	conn, err := h.getConn(checkCtx)
	if err != nil {
		status.Error = fmt.Sprintf("connect: %v", err)
		h.logger.Warn("health check failed: cannot connect", "error", err)
		duration := time.Since(startTime)
		h.setStatus(status, duration)
		h.recordFailure()
		healthCheckTotal.WithLabelValues("error").Inc()
		healthCheckDuration.Observe(duration.Seconds())
		return
	}
	// Note: connection is reused, not closed after each check.

	status.AcceptingConns = true

	// Check pg_is_in_recovery().
	var isInRecovery bool
	if err := conn.QueryRow(checkCtx, "SELECT pg_is_in_recovery()").Scan(&isInRecovery); err != nil {
		status.Error = fmt.Sprintf("pg_is_in_recovery: %v", err)
		h.logger.Warn("health check failed: pg_is_in_recovery", "error", err)
		duration := time.Since(startTime)
		h.setStatus(status, duration)
		h.recordFailure()
		healthCheckTotal.WithLabelValues("error").Inc()
		healthCheckDuration.Observe(duration.Seconds())
		return
	}
	status.IsInRecovery = isInRecovery
	status.Healthy = true
	h.recordSuccess()

	// If replica, check replication lag.
	if isInRecovery {
		var lag *int64
		err := conn.QueryRow(checkCtx,
			"SELECT pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())::bigint",
		).Scan(&lag)
		if err == nil && lag != nil {
			status.ReplicationLag = *lag
			replicationLagBytes.Set(float64(*lag))
		}
	} else {
		// If primary, check pg_stat_replication for replica info.
		rows, err := conn.Query(checkCtx, `
			SELECT
				coalesce(application_name, ''),
				coalesce(state, ''),
				coalesce(pg_wal_lsn_diff(sent_lsn, write_lsn)::bigint, 0),
				coalesce(pg_wal_lsn_diff(sent_lsn, flush_lsn)::bigint, 0),
				coalesce(pg_wal_lsn_diff(sent_lsn, replay_lsn)::bigint, 0),
				coalesce(sync_state, '')
			FROM pg_stat_replication
		`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var r ReplicaInfo
				if err := rows.Scan(&r.Name, &r.State, &r.WriteLag, &r.FlushLag, &r.ReplayLag, &r.SyncState); err != nil {
					continue
				}
				status.Replicas = append(status.Replicas, r)
			}
		}

		// Check replication slots.
		slotRows, err := conn.Query(checkCtx, `
			SELECT
				slot_name,
				coalesce(slot_type, ''),
				active,
				coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint, 0)
			FROM pg_replication_slots
		`)
		if err == nil {
			defer slotRows.Close()
			for slotRows.Next() {
				var s SlotInfo
				if err := slotRows.Scan(&s.Name, &s.Type, &s.Active, &s.WALBytes); err != nil {
					continue
				}
				status.ReplicationSlots = append(status.ReplicationSlots, s)
			}
		}
	}

	// Active connections count.
	var activeConns int
	if err := conn.QueryRow(checkCtx, "SELECT count(*) FROM pg_stat_activity WHERE state = 'active'").Scan(&activeConns); err == nil {
		status.ActiveConns = activeConns
	}

	h.logger.Debug("health check complete",
		"healthy", status.Healthy,
		"is_recovery", status.IsInRecovery,
		"lag", status.ReplicationLag,
		"replicas", len(status.Replicas),
	)

	duration := time.Since(startTime)
	h.setStatus(status, duration)
	healthCheckTotal.WithLabelValues("success").Inc()
	healthCheckDuration.Observe(duration.Seconds())
}

func (h *HealthChecker) setStatus(s HealthStatus, duration time.Duration) {
	h.mu.Lock()
	h.status = s
	h.mu.Unlock()

	// Add to history.
	if h.history != nil {
		h.history.Add(HealthCheckEntry{
			Status:    s,
			Timestamp: s.LastCheck,
			Duration:  duration,
		})
	}

	h.readyOnce.Do(func() { close(h.readyCh) })
}

// recordFailure increments the failure counter and increases the backoff interval.
func (h *HealthChecker) recordFailure() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.consecutiveFailures++

	// Calculate new interval with exponential backoff.
	newInterval := time.Duration(float64(h.interval) * h.backoffMultiplier * float64(h.consecutiveFailures))
	if newInterval > h.maxBackoff {
		newInterval = h.maxBackoff
	}

	if newInterval != h.currentInterval {
		h.logger.Info("health check backoff increased",
			"failures", h.consecutiveFailures,
			"interval", newInterval,
		)
		h.currentInterval = newInterval
	}
}

// recordSuccess resets the failure counter and restores the base interval.
func (h *HealthChecker) recordSuccess() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.consecutiveFailures > 0 {
		h.logger.Info("health check recovered",
			"failures_cleared", h.consecutiveFailures,
			"interval", h.interval,
		)
		h.consecutiveFailures = 0
		h.currentInterval = h.interval
	}
}
