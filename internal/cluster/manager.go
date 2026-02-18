package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/jrumley/pgbastion/internal/config"
	"github.com/jrumley/pgbastion/internal/consensus"
)

// NodeState represents the state of this node in the cluster state machine.
type NodeState string

const (
	StateInitializing       NodeState = "INITIALIZING"
	StateRunningPrimary     NodeState = "RUNNING_PRIMARY"
	StateRunningReplica     NodeState = "RUNNING_REPLICA"
	StateFailoverInProgress NodeState = "FAILOVER_IN_PROGRESS"
	StateDemoting           NodeState = "DEMOTING"
	StatePromoting          NodeState = "PROMOTING"
	StateReplicaLagging     NodeState = "REPLICA_LAGGING"
	StateRecovering         NodeState = "RECOVERING"
	StateFenced             NodeState = "FENCED"
)

// Manager orchestrates cluster management for the local PostgreSQL instance.
type Manager struct {
	cfg           *config.Config
	store         *consensus.Store
	healthChecker *HealthChecker
	failover      *FailoverController
	replication   *ReplicationManager
	executor      CommandExecutor
	logger        *slog.Logger

	mu    sync.RWMutex
	state NodeState

	paused bool

	// raftErrorCount tracks consecutive Raft write failures for partition detection.
	raftErrorCount int
	// raftErrorBackoff is the current backoff duration after Raft errors.
	raftErrorBackoff time.Duration
	// lastRaftError is the time of the last Raft write error.
	lastRaftError time.Time
}

// NewManager creates a new cluster Manager.
func NewManager(
	cfg *config.Config,
	store *consensus.Store,
	healthChecker *HealthChecker,
	failover *FailoverController,
	replication *ReplicationManager,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		cfg:           cfg,
		store:         store,
		healthChecker: healthChecker,
		failover:      failover,
		replication:   replication,
		executor:      &realExecutor{},
		logger:        logger.With("component", "cluster-manager"),
		state:         StateInitializing,
	}
}

// NewManagerWithExecutor creates a Manager with a custom executor (for testing).
func NewManagerWithExecutor(
	cfg *config.Config,
	store *consensus.Store,
	healthChecker *HealthChecker,
	failover *FailoverController,
	replication *ReplicationManager,
	executor CommandExecutor,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		cfg:           cfg,
		store:         store,
		healthChecker: healthChecker,
		failover:      failover,
		replication:   replication,
		executor:      executor,
		logger:        logger.With("component", "cluster-manager"),
		state:         StateInitializing,
	}
}

// State returns the current node state.
func (m *Manager) State() NodeState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// IsPaused returns whether automatic failover is paused.
func (m *Manager) IsPaused() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.paused
}

// Pause pauses automatic failover.
func (m *Manager) Pause() {
	m.mu.Lock()
	m.paused = true
	m.mu.Unlock()
	m.logger.Info("automatic failover paused")
}

// Resume resumes automatic failover.
func (m *Manager) Resume() {
	m.mu.Lock()
	m.paused = false
	m.mu.Unlock()
	m.logger.Info("automatic failover resumed")
}

// recordRaftError tracks Raft write failures and applies backoff.
func (m *Manager) recordRaftError(err error) {
	if err == nil {
		m.mu.Lock()
		m.raftErrorCount = 0
		m.raftErrorBackoff = 0
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	m.raftErrorCount++
	m.lastRaftError = time.Now()
	if m.raftErrorBackoff == 0 {
		m.raftErrorBackoff = time.Second
	} else if m.raftErrorBackoff < 30*time.Second {
		m.raftErrorBackoff *= 2
	}
	backoff := m.raftErrorBackoff
	count := m.raftErrorCount
	m.mu.Unlock()

	m.logger.Warn("raft write error, backing off",
		"error", err,
		"consecutive_errors", count,
		"backoff", backoff,
	)
}

// isRaftBackoff returns true if we should skip Raft-dependent operations due to recent errors.
func (m *Manager) isRaftBackoff() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.raftErrorCount == 0 {
		return false
	}
	return time.Since(m.lastRaftError) < m.raftErrorBackoff
}

func (m *Manager) setState(newState NodeState) {
	m.mu.Lock()
	oldState := m.state
	m.state = newState
	m.mu.Unlock()

	if oldState != newState {
		m.logger.Info("state transition",
			"from", string(oldState),
			"to", string(newState),
		)

		// Write state to Raft FSM.
		if m.store != nil {
			if err := m.store.UpdateNodeHealth(consensus.NodeInfo{
				Name:     m.cfg.Node.Name,
				State:    string(newState),
				LastSeen: time.Now(),
			}); err != nil {
				m.logger.Debug("failed to write state to consensus", "error", err)
			}
		}
	}
}

// Run starts the cluster manager loop. Blocks until context is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	m.logger.Info("starting cluster manager", "node", m.cfg.Node.Name)

	// Start health checker in background.
	go m.healthChecker.Run(ctx)

	// Wait for first health check to be ready.
	select {
	case <-m.healthChecker.Ready():
		m.logger.Debug("health checker ready")
	case <-time.After(m.healthChecker.interval + 5*time.Second):
		m.logger.Warn("timed out waiting for first health check, proceeding")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Main loop: evaluate state transitions based on health checks.
	ticker := time.NewTicker(m.healthChecker.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("cluster manager shutting down")
			return ctx.Err()
		case <-ticker.C:
			m.evaluate(ctx)
		}
	}
}

func (m *Manager) evaluate(ctx context.Context) {
	// Process any pending directives targeting this node.
	m.processDirectives(ctx)

	// Monitor in-flight failover directives (Raft leader only).
	if m.failover != nil && m.store != nil && m.store.IsLeader() {
		if m.failover.MonitorDirectives() {
			// Failover completed — state will be updated by health checks.
			m.logger.Info("failover operation completed")
		}
	}

	status := m.healthChecker.Status()
	currentState := m.State()

	switch currentState {
	case StateInitializing:
		if !status.Healthy {
			return
		}
		if status.IsInRecovery {
			m.setState(StateRunningReplica)
		} else {
			m.setState(StateRunningPrimary)
		}

	case StateRunningPrimary:
		if !status.Healthy {
			m.logger.Warn("primary unhealthy, entering recovery state")
			m.setState(StateRecovering)
			return
		}
		if status.IsInRecovery {
			// We were primary but are now in recovery — demoted externally.
			m.logger.Warn("primary detected in recovery mode, transitioning to replica")
			m.setState(StateRunningReplica)
		}

	case StateRunningReplica:
		if !status.Healthy {
			m.logger.Warn("replica unhealthy, entering recovery state")
			m.setState(StateRecovering)
			return
		}
		if !status.IsInRecovery {
			// We were a replica but are no longer in recovery — promoted.
			m.logger.Info("replica no longer in recovery, transitioning to primary")
			m.setState(StateRunningPrimary)
			return
		}
		// Check for excessive lag.
		if m.cfg.Failover.MaxLagThreshold > 0 && status.ReplicationLag > m.cfg.Failover.MaxLagThreshold {
			m.logger.Warn("replica lagging beyond threshold",
				"lag", status.ReplicationLag,
				"threshold", m.cfg.Failover.MaxLagThreshold,
			)
			m.setState(StateReplicaLagging)
		}

		// If we are the Raft leader and the primary is unhealthy, consider failover.
		// Skip if we're in Raft backoff due to recent write errors (partition detection).
		if m.store != nil && m.store.IsLeader() && !m.IsPaused() && !m.isRaftBackoff() {
			m.checkPrimaryHealth(ctx)
		}

	case StateReplicaLagging:
		if !status.Healthy {
			m.setState(StateRecovering)
			return
		}
		if m.cfg.Failover.MaxLagThreshold == 0 || status.ReplicationLag <= m.cfg.Failover.MaxLagThreshold {
			m.setState(StateRunningReplica)
		}

	case StateRecovering:
		if status.Healthy {
			if status.IsInRecovery {
				m.setState(StateRunningReplica)
			} else {
				m.setState(StateRunningPrimary)
			}
		}

	case StateFenced:
		// Fenced nodes stay fenced until manually unfenced.
		return

	case StateFailoverInProgress, StateDemoting, StatePromoting:
		// These are transient states managed by the failover controller.
		return
	}
}

// processDirectives reads pending directives for this node from the Raft FSM and executes them locally.
func (m *Manager) processDirectives(ctx context.Context) {
	if m.store == nil {
		return
	}

	directives := m.store.GetDirectivesForNode(m.cfg.Node.Name)
	for _, d := range directives {
		m.logger.Info("processing directive", "id", d.ID, "type", d.Type)
		var errMsg string
		if err := m.executeDirective(ctx, d); err != nil {
			m.logger.Error("directive execution failed", "id", d.ID, "type", d.Type, "error", err)
			errMsg = err.Error()
		} else {
			m.logger.Info("directive executed successfully", "id", d.ID, "type", d.Type)
		}

		// Complete directive with retry.
		m.completeDirectiveWithRetry(ctx, d.ID, errMsg)
	}
}

// completeDirectiveWithRetry attempts to complete a directive with exponential backoff.
// This handles transient Raft leadership changes gracefully.
func (m *Manager) completeDirectiveWithRetry(ctx context.Context, directiveID, errMsg string) {
	const maxRetries = 5
	backoff := 100 * time.Millisecond

	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := m.store.CompleteDirective(directiveID, errMsg)
		if err != nil {
			// Track Raft errors for partition detection.
			if err == consensus.ErrNotLeader {
				m.recordRaftError(err)
			}
			if attempt == maxRetries {
				m.logger.Error("failed to complete directive after retries",
					"directive_id", directiveID,
					"attempts", attempt,
					"error", err,
				)
				return
			}
			m.logger.Warn("retrying directive completion",
				"directive_id", directiveID,
				"attempt", attempt,
				"backoff", backoff,
				"error", err,
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff *= 2 // Exponential backoff.
			}
		} else {
			// Clear error count on success.
			m.recordRaftError(nil)
			if attempt > 1 {
				m.logger.Info("directive completed after retry",
					"directive_id", directiveID,
					"attempts", attempt,
				)
			}
			return
		}
	}
}

func (m *Manager) executeDirective(ctx context.Context, d *consensus.Directive) error {
	pgCtl := filepath.Join(m.cfg.PostgreSQL.BinDir, "pg_ctl")

	switch d.Type {
	case consensus.DirectiveFence:
		m.setState(StateFenced)
		output, err := m.executor.Run(ctx, pgCtl, "stop", "-D", m.cfg.PostgreSQL.DataDir, "-m", "fast")
		if err != nil {
			return fmt.Errorf("pg_ctl stop: %s: %w", string(output), err)
		}
		return nil

	case consensus.DirectivePromote:
		m.setState(StatePromoting)
		output, err := m.executor.Run(ctx, pgCtl, "promote", "-D", m.cfg.PostgreSQL.DataDir)
		if err != nil {
			return fmt.Errorf("pg_ctl promote: %s: %w", string(output), err)
		}
		// Update FSM with new primary info.
		if m.store != nil {
			if err := m.store.SetPrimary(consensus.NodeInfo{
				Name:     m.cfg.Node.Name,
				Host:     m.cfg.Node.Address,
				Port:     m.cfg.PostgreSQL.Port,
				State:    "running",
				LastSeen: time.Now(),
			}); err != nil {
				m.logger.Debug("failed to update primary in consensus after promote", "error", err)
			}
		}
		m.setState(StateRunningPrimary)
		return nil

	case consensus.DirectiveDemote:
		m.setState(StateDemoting)
		output, err := m.executor.Run(ctx, pgCtl, "stop", "-D", m.cfg.PostgreSQL.DataDir, "-m", "fast")
		if err != nil {
			return fmt.Errorf("pg_ctl stop for demote: %s: %w", string(output), err)
		}
		// Create standby.signal for restart as replica.
		signalPath := filepath.Join(m.cfg.PostgreSQL.DataDir, "standby.signal")
		if _, err := m.executor.Run(ctx, "touch", signalPath); err != nil {
			return fmt.Errorf("creating standby.signal: %w", err)
		}
		// Restart PostgreSQL.
		output, err = m.executor.Run(ctx, pgCtl, "start", "-D", m.cfg.PostgreSQL.DataDir)
		if err != nil {
			return fmt.Errorf("pg_ctl start after demote: %s: %w", string(output), err)
		}
		m.setState(StateRunningReplica)
		return nil

	case consensus.DirectiveCheckpoint:
		// Run CHECKPOINT via pg_ctl or psql. Use the executor to call psql.
		psql := filepath.Join(m.cfg.PostgreSQL.BinDir, "psql")
		output, err := m.executor.Run(ctx, psql, "-h", "localhost", "-p", fmt.Sprintf("%d", m.cfg.PostgreSQL.Port),
			"-U", m.cfg.PostgreSQL.Superuser, "-c", "CHECKPOINT")
		if err != nil {
			return fmt.Errorf("CHECKPOINT: %s: %w", string(output), err)
		}
		return nil

	case consensus.DirectiveRewind:
		pgRewind := filepath.Join(m.cfg.PostgreSQL.BinDir, "pg_rewind")
		targetPrimary := d.Params["target_primary"]
		if targetPrimary == "" {
			return fmt.Errorf("pg_rewind: target_primary param required")
		}
		sourceStr := fmt.Sprintf("host=%s port=%d user=%s", targetPrimary, m.cfg.PostgreSQL.Port, m.cfg.PostgreSQL.Superuser)
		output, err := m.executor.Run(ctx, pgRewind, "--target-pgdata="+m.cfg.PostgreSQL.DataDir, "--source-server="+sourceStr)
		if err != nil {
			return fmt.Errorf("pg_rewind: %s: %w", string(output), err)
		}
		return nil

	case consensus.DirectiveBasebackup:
		primaryHost := d.Params["primary_host"]
		if primaryHost == "" {
			return fmt.Errorf("pg_basebackup: primary_host param required")
		}
		pgBasebackup := filepath.Join(m.cfg.PostgreSQL.BinDir, "pg_basebackup")
		output, err := m.executor.Run(ctx, pgBasebackup,
			"-h", primaryHost,
			"-p", fmt.Sprintf("%d", m.cfg.PostgreSQL.Port),
			"-U", m.cfg.PostgreSQL.ReplicationUser,
			"-D", m.cfg.PostgreSQL.DataDir,
			"-Fp", "-Xs", "-P", "-R",
		)
		if err != nil {
			return fmt.Errorf("pg_basebackup: %s: %w", string(output), err)
		}
		return nil

	default:
		return fmt.Errorf("unknown directive type: %s", d.Type)
	}
}

// checkPrimaryHealth checks whether the primary is healthy from the Raft leader's perspective.
func (m *Manager) checkPrimaryHealth(ctx context.Context) {
	if m.failover == nil {
		return
	}

	fsm := m.store.FSM()
	primary := fsm.GetPrimary()
	if primary == nil {
		return
	}

	// If the primary node is this node, skip (we already handle local health).
	if primary.Name == m.cfg.Node.Name {
		return
	}

	// Check if primary's health data in FSM is stale.
	nodes := fsm.GetAllNodes()
	primaryNode, ok := nodes[primary.Name]
	if !ok {
		return
	}

	staleness := time.Since(primaryNode.LastSeen)
	if staleness > m.cfg.Failover.ConfirmationPeriod {
		m.logger.Warn("primary node appears unhealthy (stale health data)",
			"primary", primary.Name,
			"last_seen", primaryNode.LastSeen,
			"staleness", staleness,
		)

		if m.failover.ShouldFailover(primaryNode) {
			m.logger.Info("initiating automatic failover",
				"failed_primary", primary.Name,
			)
			m.setState(StateFailoverInProgress)
			if err := m.failover.ExecuteFailover(ctx, primary.Name); err != nil {
				m.logger.Error("failover failed", "error", err)
				m.setState(StateRunningReplica)
			}
		}
	}
}

// ManualFailover triggers a manual failover to the specified target node.
func (m *Manager) ManualFailover(ctx context.Context, targetNode string) error {
	if m.failover == nil {
		return fmt.Errorf("failover controller not initialized")
	}
	if m.store == nil || !m.store.IsLeader() {
		return fmt.Errorf("failover can only be initiated from the Raft leader")
	}

	fsm := m.store.FSM()
	primary := fsm.GetPrimary()
	if primary == nil {
		return fmt.Errorf("no current primary known")
	}

	m.logger.Info("manual failover initiated",
		"from", primary.Name,
		"to", targetNode,
	)
	m.setState(StateFailoverInProgress)

	if err := m.failover.ExecuteFailoverTo(ctx, primary.Name, targetNode); err != nil {
		m.setState(StateRunningReplica)
		return fmt.Errorf("failover failed: %w", err)
	}

	return nil
}

// ManualSwitchover triggers a graceful switchover to the specified target node.
func (m *Manager) ManualSwitchover(ctx context.Context, targetNode string) error {
	if m.failover == nil {
		return fmt.Errorf("failover controller not initialized")
	}
	if m.store == nil || !m.store.IsLeader() {
		return fmt.Errorf("switchover can only be initiated from the Raft leader")
	}

	fsm := m.store.FSM()
	primary := fsm.GetPrimary()
	if primary == nil {
		return fmt.Errorf("no current primary known")
	}

	m.logger.Info("manual switchover initiated",
		"from", primary.Name,
		"to", targetNode,
	)
	m.setState(StateFailoverInProgress)

	return m.failover.ExecuteSwitchover(ctx, primary.Name, targetNode)
}
