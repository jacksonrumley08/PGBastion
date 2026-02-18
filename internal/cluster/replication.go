package cluster

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/jrumley/pgbastion/internal/config"
	"github.com/jrumley/pgbastion/internal/consensus"
)

// ReplicationManager handles replication slot management and config templating.
type ReplicationManager struct {
	cfg      *config.Config
	store    *consensus.Store
	executor CommandExecutor
	logger   *slog.Logger
}

// NewReplicationManager creates a new ReplicationManager.
func NewReplicationManager(cfg *config.Config, store *consensus.Store, logger *slog.Logger) *ReplicationManager {
	return &ReplicationManager{
		cfg:      cfg,
		store:    store,
		executor: &realExecutor{},
		logger:   logger.With("component", "replication"),
	}
}

// NewReplicationManagerWithExecutor creates a ReplicationManager with a custom executor (for testing).
func NewReplicationManagerWithExecutor(cfg *config.Config, store *consensus.Store, executor CommandExecutor, logger *slog.Logger) *ReplicationManager {
	return &ReplicationManager{
		cfg:      cfg,
		store:    store,
		executor: executor,
		logger:   logger.With("component", "replication"),
	}
}

// CreateReplicationSlot creates a physical replication slot via SQL.
func (rm *ReplicationManager) CreateReplicationSlot(ctx context.Context, conn PGConn, slotName string) error {
	var slotNameOut string
	err := conn.QueryRow(ctx, "SELECT slot_name FROM pg_create_physical_replication_slot($1, true)", slotName).Scan(&slotNameOut)
	if err != nil {
		return fmt.Errorf("creating replication slot %s: %w", slotName, err)
	}
	rm.logger.Info("created replication slot", "slot", slotName)
	return nil
}

// DropReplicationSlot drops a replication slot via SQL.
func (rm *ReplicationManager) DropReplicationSlot(ctx context.Context, conn PGConn, slotName string) error {
	var result bool
	// pg_drop_replication_slot returns void, so we wrap in a SELECT true
	err := conn.QueryRow(ctx, "SELECT pg_drop_replication_slot($1) IS NULL", slotName).Scan(&result)
	if err != nil {
		return fmt.Errorf("dropping replication slot %s: %w", slotName, err)
	}
	rm.logger.Info("dropped replication slot", "slot", slotName)
	return nil
}

// CleanupOrphanedSlots drops replication slots that don't correspond to known cluster nodes.
func (rm *ReplicationManager) CleanupOrphanedSlots(ctx context.Context, conn PGConn) error {
	if rm.store == nil {
		return nil
	}

	knownNodes := rm.store.FSM().GetAllNodes()

	rows, err := conn.Query(ctx, "SELECT slot_name, active FROM pg_replication_slots WHERE slot_type = 'physical'")
	if err != nil {
		return fmt.Errorf("querying replication slots: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var slotName string
		var active bool
		if err := rows.Scan(&slotName, &active); err != nil {
			continue
		}

		if _, known := knownNodes[slotName]; !known && !active {
			rm.logger.Warn("dropping orphaned replication slot", "slot", slotName)
			if err := rm.DropReplicationSlot(ctx, conn, slotName); err != nil {
				rm.logger.Error("failed to drop orphaned slot", "slot", slotName, "error", err)
			}
		}
	}

	return nil
}

// pgHBAMarkerBegin marks the start of pgbastion-managed entries.
const pgHBAMarkerBegin = "# BEGIN pgbastion managed block - do not edit between markers"

// pgHBAMarkerEnd marks the end of pgbastion-managed entries.
const pgHBAMarkerEnd = "# END pgbastion managed block"

// pgHBATemplate is the template for pg_hba.conf entries for replication.
const pgHBATemplate = `# TYPE  DATABASE        USER            ADDRESS                 METHOD
local   all             {{.Superuser}}                          trust
host    all             {{.Superuser}}  127.0.0.1/32            trust
host    all             {{.Superuser}}  ::1/128                 trust
{{range .Nodes -}}
host    replication     {{$.ReplicationUser}}  {{.Host}}/32    trust
host    all             {{$.Superuser}}        {{.Host}}/32    trust
{{end -}}
`

// PGHBAData holds template data for pg_hba.conf generation.
type PGHBAData struct {
	Superuser       string
	ReplicationUser string
	Nodes           []nodeAddr
}

type nodeAddr struct {
	Host string
}

// WritePGHBA generates and writes a pg_hba.conf file based on current cluster membership.
// It preserves custom entries outside the pgbastion-managed block.
func (rm *ReplicationManager) WritePGHBA(ctx context.Context) error {
	if rm.store == nil {
		return fmt.Errorf("no consensus store available")
	}

	nodes := rm.store.FSM().GetAllNodes()
	data := PGHBAData{
		Superuser:       rm.cfg.PostgreSQL.Superuser,
		ReplicationUser: rm.cfg.PostgreSQL.ReplicationUser,
	}
	for _, n := range nodes {
		data.Nodes = append(data.Nodes, nodeAddr{Host: n.Host})
	}

	tmpl, err := template.New("pg_hba").Parse(pgHBATemplate)
	if err != nil {
		return fmt.Errorf("parsing pg_hba template: %w", err)
	}

	// Render the managed block content.
	var managedBlock bytes.Buffer
	if err := tmpl.Execute(&managedBlock, data); err != nil {
		return fmt.Errorf("executing pg_hba template: %w", err)
	}

	hbaPath := filepath.Join(rm.cfg.PostgreSQL.DataDir, "pg_hba.conf")
	backupPath := hbaPath + ".bak"

	var existingContent []byte
	var fileExists bool

	// Read existing pg_hba.conf if it exists.
	if info, err := os.Stat(hbaPath); err == nil && !info.IsDir() {
		fileExists = true
		existingContent, err = os.ReadFile(hbaPath)
		if err != nil {
			rm.logger.Warn("failed to read pg_hba.conf", "error", err)
			existingContent = nil
		} else {
			// Backup existing file.
			if err := os.WriteFile(backupPath, existingContent, 0600); err != nil {
				rm.logger.Warn("failed to backup pg_hba.conf", "error", err)
			} else {
				rm.logger.Debug("backed up pg_hba.conf", "backup", backupPath)
			}
		}
	}

	var newContent string

	if len(existingContent) > 0 {
		content := string(existingContent)
		beginIdx := strings.Index(content, pgHBAMarkerBegin)
		endIdx := strings.Index(content, pgHBAMarkerEnd)

		if beginIdx >= 0 && endIdx > beginIdx {
			// Found existing markers - replace the managed block.
			before := content[:beginIdx]
			after := content[endIdx+len(pgHBAMarkerEnd):]
			newContent = before + pgHBAMarkerBegin + "\n" + managedBlock.String() + pgHBAMarkerEnd + after
			rm.logger.Debug("replacing existing pgbastion managed block")
		} else if beginIdx >= 0 || endIdx >= 0 {
			// Partial markers found - log warning and append fresh block at end.
			rm.logger.Warn("found partial pgbastion markers in pg_hba.conf, appending fresh block")
			newContent = content + "\n" + pgHBAMarkerBegin + "\n" + managedBlock.String() + pgHBAMarkerEnd + "\n"
		} else {
			// No markers found - append our block at the end.
			rm.logger.Info("no existing pgbastion markers found, appending managed block")
			newContent = content
			if !strings.HasSuffix(content, "\n") {
				newContent += "\n"
			}
			newContent += "\n" + pgHBAMarkerBegin + "\n" + managedBlock.String() + pgHBAMarkerEnd + "\n"
		}
	} else {
		// No existing file or empty - create new with markers.
		if fileExists {
			rm.logger.Info("existing pg_hba.conf is empty, creating new content")
		}
		newContent = pgHBAMarkerBegin + "\n" + managedBlock.String() + pgHBAMarkerEnd + "\n"
	}

	f, err := os.Create(hbaPath)
	if err != nil {
		return fmt.Errorf("creating pg_hba.conf: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(newContent); err != nil {
		return fmt.Errorf("writing pg_hba.conf: %w", err)
	}

	rm.logger.Info("wrote pg_hba.conf", "path", hbaPath, "nodes", len(data.Nodes))
	return nil
}

// ReloadPostgreSQL sends a SIGHUP to PostgreSQL to reload configuration.
func (rm *ReplicationManager) ReloadPostgreSQL(ctx context.Context) error {
	pgCtl := filepath.Join(rm.cfg.PostgreSQL.BinDir, "pg_ctl")
	output, err := rm.executor.Run(ctx, pgCtl, "reload", "-D", rm.cfg.PostgreSQL.DataDir)
	if err != nil {
		return fmt.Errorf("pg_ctl reload: %s: %w", string(output), err)
	}
	rm.logger.Info("reloaded PostgreSQL configuration")
	return nil
}

// Reinitialize reinitializes a replica from the primary using pg_basebackup.
func (rm *ReplicationManager) Reinitialize(ctx context.Context, primaryHost string) error {
	rm.logger.Info("reinitializing replica from primary", "primary", primaryHost)

	pgBasebackup := filepath.Join(rm.cfg.PostgreSQL.BinDir, "pg_basebackup")
	output, err := rm.executor.Run(ctx,
		pgBasebackup,
		"-h", primaryHost,
		"-p", fmt.Sprintf("%d", rm.cfg.PostgreSQL.Port),
		"-U", rm.cfg.PostgreSQL.ReplicationUser,
		"-D", rm.cfg.PostgreSQL.DataDir,
		"-Fp", "-Xs", "-P", "-R",
	)
	if err != nil {
		return fmt.Errorf("pg_basebackup: %s: %w", string(output), err)
	}

	rm.logger.Info("reinitialization complete")
	return nil
}
