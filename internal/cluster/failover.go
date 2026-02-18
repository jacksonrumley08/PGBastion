package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/jrumley/pgbastion/internal/config"
	"github.com/jrumley/pgbastion/internal/consensus"
)

var (
	failoverTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_failover_total",
		Help: "Total number of failover operations",
	}, []string{"type", "result"})

	failoverDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pgbastion_failover_duration_seconds",
		Help:    "Duration of failover operations in seconds",
		Buckets: prometheus.ExponentialBuckets(1, 2, 8), // 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s
	})

	directivesPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_directives_published_total",
		Help: "Total number of directives published",
	}, []string{"type"})
)

// CommandExecutor abstracts shell command execution for testing.
type CommandExecutor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// realExecutor runs actual shell commands.
type realExecutor struct{}

func (e *realExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// DefaultDirectiveTimeout is the default timeout for directive completion.
const DefaultDirectiveTimeout = 60 * time.Second

// DefaultSwitchoverSyncTimeout is the timeout for waiting for replica to sync during switchover.
const DefaultSwitchoverSyncTimeout = 30 * time.Second

// SwitchoverMaxLagBytes is the maximum acceptable lag (in bytes) for switchover to proceed.
// A value of 0 means replica must be fully caught up.
const SwitchoverMaxLagBytes = 0

// FailoverController manages failover and switchover operations.
// In the agent-based model, it publishes directives to the Raft FSM
// rather than executing pg_ctl commands directly.
type FailoverController struct {
	cfgMu    sync.RWMutex
	cfg      *config.Config
	store    *consensus.Store
	executor CommandExecutor
	logger   *slog.Logger

	mu               sync.Mutex
	failoverInFlight bool

	// activeDirectiveIDs tracks directive IDs for the current failover operation.
	activeDirectiveIDs []string

	// failoverStartTime tracks when the current failover started (for timeout).
	failoverStartTime time.Time

	// confirmationStart tracks when we first noticed the primary was unhealthy.
	confirmationStart time.Time
}

// UpdateConfig hot-reloads the failover configuration.
func (f *FailoverController) UpdateConfig(cfg *config.Config) {
	f.cfgMu.Lock()
	f.cfg = cfg
	f.cfgMu.Unlock()
	f.logger.Info("failover configuration updated",
		"confirmation_period", cfg.Failover.ConfirmationPeriod,
		"fencing_enabled", cfg.Failover.IsFencingEnabled(),
	)
}

func (f *FailoverController) getConfig() *config.Config {
	f.cfgMu.RLock()
	defer f.cfgMu.RUnlock()
	return f.cfg
}

// NewFailoverController creates a new FailoverController.
func NewFailoverController(cfg *config.Config, store *consensus.Store, logger *slog.Logger) *FailoverController {
	return &FailoverController{
		cfg:      cfg,
		store:    store,
		executor: &realExecutor{},
		logger:   logger.With("component", "failover"),
	}
}

// NewFailoverControllerWithExecutor creates a FailoverController with a custom executor (for testing).
func NewFailoverControllerWithExecutor(cfg *config.Config, store *consensus.Store, executor CommandExecutor, logger *slog.Logger) *FailoverController {
	return &FailoverController{
		cfg:      cfg,
		store:    store,
		executor: executor,
		logger:   logger.With("component", "failover"),
	}
}

// IsFailoverInFlight returns true if a failover is currently in progress.
func (f *FailoverController) IsFailoverInFlight() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failoverInFlight
}

// ShouldFailover returns true if conditions are met for automatic failover.
func (f *FailoverController) ShouldFailover(primaryNode *consensus.NodeInfo) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failoverInFlight {
		return false
	}

	staleness := time.Since(primaryNode.LastSeen)

	if f.confirmationStart.IsZero() {
		// First detection — start confirmation timer.
		f.confirmationStart = time.Now()
		f.logger.Info("primary unhealthy, starting confirmation period",
			"primary", primaryNode.Name,
			"staleness", staleness,
		)
		return false
	}

	// Check if confirmation period has elapsed.
	elapsed := time.Since(f.confirmationStart)
	cfg := f.getConfig()
	if elapsed < cfg.Failover.ConfirmationPeriod {
		return false
	}

	f.logger.Warn("confirmation period elapsed, failover recommended",
		"primary", primaryNode.Name,
		"confirmation_elapsed", elapsed,
	)
	return true
}

// ExecuteFailover publishes directives for automatic failover:
// FENCE directive targeting the failed primary, PROMOTE directive targeting the best replica.
func (f *FailoverController) ExecuteFailover(ctx context.Context, failedPrimary string) error {
	f.mu.Lock()
	f.failoverInFlight = true
	f.activeDirectiveIDs = nil
	f.failoverStartTime = time.Now()
	f.mu.Unlock()

	// Select best replica (smallest lag).
	target, err := f.selectBestReplica(failedPrimary)
	if err != nil {
		f.clearFailover()
		return fmt.Errorf("selecting failover target: %w", err)
	}

	f.logger.Info("failover target selected", "target", target.Name, "lag", target.Lag)

	return f.publishFailoverDirectives(failedPrimary, target.Name)
}

// ExecuteFailoverTo publishes directives for a manual failover to a specific target node.
func (f *FailoverController) ExecuteFailoverTo(ctx context.Context, failedPrimary, targetNode string) error {
	f.mu.Lock()
	f.failoverInFlight = true
	f.activeDirectiveIDs = nil
	f.failoverStartTime = time.Now()
	f.mu.Unlock()

	return f.publishFailoverDirectives(failedPrimary, targetNode)
}

func (f *FailoverController) publishFailoverDirectives(failedPrimary, targetNode string) error {
	if f.store == nil {
		f.clearFailover()
		return fmt.Errorf("no consensus store available")
	}

	now := time.Now()
	var directiveIDs []string

	// Step 1: Publish FENCE directive for the failed primary (if fencing is enabled).
	cfg := f.getConfig()
	if cfg.Failover.IsFencingEnabled() {
		fenceID := fmt.Sprintf("fence-%s-%d", failedPrimary, now.UnixNano())
		fenceDirective := consensus.Directive{
			ID:         fenceID,
			Type:       consensus.DirectiveFence,
			TargetNode: failedPrimary,
			Status:     consensus.DirectiveStatusPending,
			CreatedAt:  now,
		}
		if err := f.store.PublishDirective(fenceDirective); err != nil {
			f.logger.Error("failed to publish FENCE directive",
				"directive_id", fenceID,
				"directive_type", consensus.DirectiveFence,
				"target_node", failedPrimary,
				"error", err,
			)
			f.clearFailover()
			return fmt.Errorf("publishing FENCE directive (id=%s, target=%s): %w", fenceID, failedPrimary, err)
		}
		f.logger.Info("published FENCE directive",
			"directive_id", fenceID,
			"target_node", failedPrimary,
		)
		directivesPublished.WithLabelValues(consensus.DirectiveFence).Inc()
		directiveIDs = append(directiveIDs, fenceID)
	}

	// Step 2: Publish PROMOTE directive for the target replica.
	promoteID := fmt.Sprintf("promote-%s-%d", targetNode, now.UnixNano())
	promoteDirective := consensus.Directive{
		ID:         promoteID,
		Type:       consensus.DirectivePromote,
		TargetNode: targetNode,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  now,
	}
	if err := f.store.PublishDirective(promoteDirective); err != nil {
		f.logger.Error("failed to publish PROMOTE directive",
			"directive_id", promoteID,
			"directive_type", consensus.DirectivePromote,
			"target_node", targetNode,
			"error", err,
		)
		f.clearFailover()
		return fmt.Errorf("publishing PROMOTE directive (id=%s, target=%s): %w", promoteID, targetNode, err)
	}
	f.logger.Info("published PROMOTE directive",
		"directive_id", promoteID,
		"target_node", targetNode,
	)
	directivesPublished.WithLabelValues(consensus.DirectivePromote).Inc()
	directiveIDs = append(directiveIDs, promoteID)

	f.mu.Lock()
	f.activeDirectiveIDs = directiveIDs
	f.mu.Unlock()

	f.logger.Info("failover directives published",
		"failed_primary", failedPrimary,
		"target", targetNode,
		"directive_count", len(directiveIDs),
	)
	failoverTotal.WithLabelValues("failover", "started").Inc()
	return nil
}

// ExecuteSwitchover performs a graceful switchover with replication sync:
// 1. CHECKPOINT on current primary (flush WAL)
// 2. Wait for target replica to catch up
// 3. DEMOTE on current primary
// 4. PROMOTE on target
func (f *FailoverController) ExecuteSwitchover(ctx context.Context, currentPrimary, targetNode string) error {
	if f.store == nil {
		return fmt.Errorf("no consensus store available")
	}

	f.mu.Lock()
	f.failoverInFlight = true
	f.activeDirectiveIDs = nil
	f.failoverStartTime = time.Now()
	f.mu.Unlock()

	now := time.Now()
	var directiveIDs []string

	// Step 1: CHECKPOINT on current primary to flush WAL.
	checkpointID := fmt.Sprintf("checkpoint-%s-%d", currentPrimary, now.UnixNano())
	if err := f.store.PublishDirective(consensus.Directive{
		ID:         checkpointID,
		Type:       consensus.DirectiveCheckpoint,
		TargetNode: currentPrimary,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  now,
	}); err != nil {
		f.clearFailover()
		return fmt.Errorf("publishing CHECKPOINT directive: %w", err)
	}
	directivesPublished.WithLabelValues(consensus.DirectiveCheckpoint).Inc()
	directiveIDs = append(directiveIDs, checkpointID)

	f.logger.Info("waiting for CHECKPOINT to complete", "directive_id", checkpointID)

	// Wait for CHECKPOINT to complete with timeout.
	checkpointCtx, checkpointCancel := context.WithTimeout(ctx, DefaultDirectiveTimeout)
	defer checkpointCancel()
	if err := f.waitForDirective(checkpointCtx, checkpointID); err != nil {
		f.clearFailover()
		return fmt.Errorf("CHECKPOINT failed: %w", err)
	}

	// Step 2: Wait for replica to sync (lag <= 0).
	f.logger.Info("waiting for replica to sync", "target", targetNode)
	syncCtx, syncCancel := context.WithTimeout(ctx, DefaultSwitchoverSyncTimeout)
	defer syncCancel()
	if err := f.waitForReplicaSync(syncCtx, targetNode, SwitchoverMaxLagBytes); err != nil {
		f.clearFailover()
		return fmt.Errorf("replica sync failed: %w", err)
	}

	// Step 3: DEMOTE on current primary.
	now = time.Now() // Refresh timestamp.
	demoteID := fmt.Sprintf("demote-%s-%d", currentPrimary, now.UnixNano())
	if err := f.store.PublishDirective(consensus.Directive{
		ID:         demoteID,
		Type:       consensus.DirectiveDemote,
		TargetNode: currentPrimary,
		Params:     map[string]string{"target_primary": targetNode},
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  now,
	}); err != nil {
		f.clearFailover()
		return fmt.Errorf("publishing DEMOTE directive: %w", err)
	}
	directivesPublished.WithLabelValues(consensus.DirectiveDemote).Inc()
	directiveIDs = append(directiveIDs, demoteID)

	// Step 4: PROMOTE on target.
	promoteID := fmt.Sprintf("promote-%s-%d", targetNode, now.UnixNano())
	if err := f.store.PublishDirective(consensus.Directive{
		ID:         promoteID,
		Type:       consensus.DirectivePromote,
		TargetNode: targetNode,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  now,
	}); err != nil {
		f.clearFailover()
		return fmt.Errorf("publishing PROMOTE directive: %w", err)
	}
	directivesPublished.WithLabelValues(consensus.DirectivePromote).Inc()
	directiveIDs = append(directiveIDs, promoteID)

	f.mu.Lock()
	f.activeDirectiveIDs = directiveIDs
	f.mu.Unlock()

	f.logger.Info("switchover directives published",
		"current_primary", currentPrimary,
		"target", targetNode,
		"directive_count", len(directiveIDs),
	)
	failoverTotal.WithLabelValues("switchover", "started").Inc()
	return nil
}

// MonitorDirectives checks whether all active directives have completed or failed.
// Returns true if the failover is done (all directives resolved or timed out).
func (f *FailoverController) MonitorDirectives() bool {
	f.mu.Lock()
	if !f.failoverInFlight || len(f.activeDirectiveIDs) == 0 {
		f.mu.Unlock()
		return false
	}
	ids := make([]string, len(f.activeDirectiveIDs))
	copy(ids, f.activeDirectiveIDs)
	startTime := f.failoverStartTime
	f.mu.Unlock()

	if f.store == nil {
		return false
	}

	// Check for timeout.
	elapsed := time.Since(startTime)
	timedOut := elapsed > DefaultDirectiveTimeout

	allDone := true
	for _, id := range ids {
		d := f.store.GetDirective(id)
		if d == nil {
			continue
		}
		switch d.Status {
		case consensus.DirectiveStatusPending:
			if timedOut {
				// Mark timed-out directives as failed.
				errMsg := fmt.Sprintf("directive timed out after %v", elapsed)
				if err := f.store.CompleteDirective(id, errMsg); err != nil {
					f.logger.Error("failed to mark directive as timed out",
						"directive_id", id,
						"directive_type", d.Type,
						"target_node", d.TargetNode,
						"error", err,
					)
				} else {
					f.logger.Error("directive timed out",
						"directive_id", id,
						"directive_type", d.Type,
						"target_node", d.TargetNode,
						"elapsed", elapsed,
					)
				}
			} else {
				allDone = false
			}
		case consensus.DirectiveStatusFailed:
			f.logger.Error("directive failed",
				"directive_id", d.ID,
				"directive_type", d.Type,
				"target_node", d.TargetNode,
				"error", d.Error,
			)
		case consensus.DirectiveStatusCompleted:
			f.logger.Info("directive completed",
				"directive_id", d.ID,
				"directive_type", d.Type,
				"target_node", d.TargetNode,
			)
		}
	}

	if allDone || timedOut {
		// Record metrics.
		failoverDuration.Observe(elapsed.Seconds())
		if timedOut {
			failoverTotal.WithLabelValues("failover", "timeout").Inc()
			f.logger.Error("failover timed out, some directives did not complete",
				"elapsed", elapsed,
				"timeout", DefaultDirectiveTimeout,
			)
		} else {
			failoverTotal.WithLabelValues("failover", "success").Inc()
			f.logger.Info("all failover directives resolved")
		}
		f.clearFailover()
		return true
	}
	return false
}

func (f *FailoverController) clearFailover() {
	f.mu.Lock()
	f.failoverInFlight = false
	f.activeDirectiveIDs = nil
	f.failoverStartTime = time.Time{}
	f.confirmationStart = time.Time{}
	f.mu.Unlock()
}

// waitForDirective waits for a directive to complete or fail.
func (f *FailoverController) waitForDirective(ctx context.Context, directiveID string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			d := f.store.GetDirective(directiveID)
			if d == nil {
				return fmt.Errorf("directive %s not found", directiveID)
			}
			switch d.Status {
			case consensus.DirectiveStatusCompleted:
				return nil
			case consensus.DirectiveStatusFailed:
				return fmt.Errorf("directive failed: %s", d.Error)
			}
			// Still pending, continue waiting.
		}
	}
}

// waitForReplicaSync waits for a replica to catch up to acceptable lag.
func (f *FailoverController) waitForReplicaSync(ctx context.Context, replicaName string, maxLag int64) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	f.logger.Info("waiting for replica to sync",
		"replica", replicaName,
		"max_lag_bytes", maxLag,
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			nodes := f.store.FSM().GetAllNodes()
			replica, ok := nodes[replicaName]
			if !ok {
				f.logger.Warn("replica not found in FSM, continuing to wait", "replica", replicaName)
				continue
			}

			if replica.Lag <= maxLag {
				f.logger.Info("replica synced",
					"replica", replicaName,
					"lag_bytes", replica.Lag,
				)
				return nil
			}

			f.logger.Debug("replica still catching up",
				"replica", replicaName,
				"lag_bytes", replica.Lag,
				"max_lag_bytes", maxLag,
			)
		}
	}
}

func (f *FailoverController) selectBestReplica(excludeName string) (*consensus.NodeInfo, error) {
	if f.store == nil {
		return nil, fmt.Errorf("no consensus store available")
	}

	replicas := f.store.FSM().GetReplicas()
	if len(replicas) == 0 {
		return nil, fmt.Errorf("no replicas available for failover")
	}

	var best *consensus.NodeInfo
	for _, r := range replicas {
		if r.Name == excludeName {
			continue
		}
		if best == nil || r.Lag < best.Lag {
			best = r
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no suitable replica found")
	}

	return best, nil
}
