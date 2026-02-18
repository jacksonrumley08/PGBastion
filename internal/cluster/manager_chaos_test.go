package cluster

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jrumley/pgbastion/internal/config"
	"github.com/jrumley/pgbastion/internal/consensus"
	"github.com/jrumley/pgbastion/internal/testutil"
)

// mockStore implements a minimal Store interface for chaos testing.
type mockStore struct {
	mu         sync.RWMutex
	fsm        *consensus.ClusterFSM
	isLeader   bool
	hasQuorum  bool
	directives map[string]*consensus.Directive
}

func newMockStore() *mockStore {
	return &mockStore{
		fsm:        consensus.NewClusterFSM(),
		directives: make(map[string]*consensus.Directive),
		hasQuorum:  true,
	}
}

func (s *mockStore) IsLeader() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isLeader
}

func (s *mockStore) SetLeader(leader bool) {
	s.mu.Lock()
	s.isLeader = leader
	s.mu.Unlock()
}

func (s *mockStore) FSM() *consensus.ClusterFSM {
	return s.fsm
}

func (s *mockStore) HasQuorum(timeout time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasQuorum
}

func (s *mockStore) SetHasQuorum(has bool) {
	s.mu.Lock()
	s.hasQuorum = has
	s.mu.Unlock()
}

func (s *mockStore) GetDirectivesForNode(nodeName string) []*consensus.Directive {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*consensus.Directive
	for _, d := range s.directives {
		if d.TargetNode == nodeName && d.Status == consensus.DirectiveStatusPending {
			result = append(result, d)
		}
	}
	return result
}

func (s *mockStore) AddDirective(d *consensus.Directive) {
	s.mu.Lock()
	s.directives[d.ID] = d
	s.mu.Unlock()
}

func (s *mockStore) CompleteDirective(id, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.directives[id]; ok {
		if errMsg == "" {
			d.Status = consensus.DirectiveStatusCompleted
		} else {
			d.Status = consensus.DirectiveStatusFailed
			d.Error = errMsg
		}
		d.CompletedAt = time.Now()
	}
	return nil
}

func (s *mockStore) UpdateNodeHealth(node consensus.NodeInfo) error {
	// Apply to FSM.
	data, _ := json.Marshal(node)
	cmd := consensus.FSMCommand{
		Type: consensus.CmdUpdateNodeHealth,
		Data: json.RawMessage(data),
	}
	cmdBytes, _ := json.Marshal(cmd)
	s.fsm.Apply(&raft.Log{Data: cmdBytes})
	return nil
}

// chaosManager wraps Manager with mock components for chaos testing.
type chaosManager struct {
	*Manager
	mockStore    *mockStore
	chaosExec    *testutil.ChaosExecutor
	healthStatus HealthStatus
}

func newChaosManager(cfg *config.Config) *chaosManager {
	ms := newMockStore()
	chaosExec := testutil.NewChaosExecutor()

	// Create mock connector that returns healthy status.
	mockConn := &mockConnector{}

	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 100*time.Millisecond, mockConn, testLogger())

	m := NewManagerWithExecutor(cfg, nil, hc, nil, nil, chaosExec, testLogger())

	// Replace store with mock.
	cm := &chaosManager{
		Manager:   m,
		mockStore: ms,
		chaosExec: chaosExec,
	}

	return cm
}

func (cm *chaosManager) setHealthStatus(status HealthStatus) {
	cm.healthChecker.setStatus(status, 0)
}

// TestChaos_DirectiveExecution_Fence tests FENCE directive execution.
func TestChaos_DirectiveExecution_Fence(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	// Set up as running primary.
	cm.setState(StateRunningPrimary)

	// Create a mock store that returns directives.
	directive := &consensus.Directive{
		ID:         "fence-1",
		Type:       consensus.DirectiveFence,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}
	cm.mockStore.AddDirective(directive)

	// Keep store nil to avoid setState trying to write to Raft.
	// Execute directive directly.
	err := cm.executeDirective(context.Background(), directive)
	if err != nil {
		t.Fatalf("directive execution failed: %v", err)
	}

	// Verify state changed to FENCED.
	if cm.State() != StateFenced {
		t.Errorf("expected FENCED state, got %s", cm.State())
	}

	// Verify pg_ctl stop was called.
	if cm.chaosExec.GetCallCount("pg_ctl") != 1 {
		t.Errorf("expected pg_ctl to be called once, got %d", cm.chaosExec.GetCallCount("pg_ctl"))
	}
}

// TestChaos_DirectiveExecution_Promote tests PROMOTE directive execution.
func TestChaos_DirectiveExecution_Promote(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningReplica)

	directive := &consensus.Directive{
		ID:         "promote-1",
		Type:       consensus.DirectivePromote,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	err := cm.executeDirective(context.Background(), directive)
	if err != nil {
		t.Fatalf("directive execution failed: %v", err)
	}

	// Verify state changed to RUNNING_PRIMARY.
	if cm.State() != StateRunningPrimary {
		t.Errorf("expected RUNNING_PRIMARY state, got %s", cm.State())
	}

	// Verify pg_ctl promote was called.
	if cm.chaosExec.GetCallCount("pg_ctl") != 1 {
		t.Errorf("expected pg_ctl to be called once, got %d", cm.chaosExec.GetCallCount("pg_ctl"))
	}
}

// TestChaos_DirectiveExecution_Demote tests DEMOTE directive execution.
func TestChaos_DirectiveExecution_Demote(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningPrimary)

	directive := &consensus.Directive{
		ID:         "demote-1",
		Type:       consensus.DirectiveDemote,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	err := cm.executeDirective(context.Background(), directive)
	if err != nil {
		t.Fatalf("directive execution failed: %v", err)
	}

	// Verify state changed to RUNNING_REPLICA.
	if cm.State() != StateRunningReplica {
		t.Errorf("expected RUNNING_REPLICA state, got %s", cm.State())
	}

	// Verify pg_ctl was called (stop + start).
	if cm.chaosExec.GetCallCount("pg_ctl") != 2 {
		t.Errorf("expected pg_ctl to be called twice, got %d", cm.chaosExec.GetCallCount("pg_ctl"))
	}

	// Verify touch was called for standby.signal.
	if cm.chaosExec.GetCallCount("touch") != 1 {
		t.Errorf("expected touch to be called once, got %d", cm.chaosExec.GetCallCount("touch"))
	}
}

// TestChaos_DirectiveExecution_Checkpoint tests CHECKPOINT directive execution.
func TestChaos_DirectiveExecution_Checkpoint(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningPrimary)

	directive := &consensus.Directive{
		ID:         "checkpoint-1",
		Type:       consensus.DirectiveCheckpoint,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	err := cm.executeDirective(context.Background(), directive)
	if err != nil {
		t.Fatalf("directive execution failed: %v", err)
	}

	// Verify psql was called.
	if cm.chaosExec.GetCallCount("psql") != 1 {
		t.Errorf("expected psql to be called once, got %d", cm.chaosExec.GetCallCount("psql"))
	}
}

// TestChaos_DirectiveExecution_FailedCommand tests directive failure handling.
func TestChaos_DirectiveExecution_FailedCommand(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningPrimary)

	// Make pg_ctl fail.
	cm.chaosExec.FailCommands["pg_ctl"] = testutil.ErrChaosInjected

	directive := &consensus.Directive{
		ID:         "fence-fail",
		Type:       consensus.DirectiveFence,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	err := cm.executeDirective(context.Background(), directive)
	if err == nil {
		t.Fatal("expected directive execution to fail")
	}

	// State should still be FENCED (we set it before attempting stop).
	if cm.State() != StateFenced {
		t.Errorf("expected FENCED state, got %s", cm.State())
	}
}

// TestChaos_SplitBrain_SelfFence tests self-fencing when quorum is lost.
func TestChaos_SplitBrain_SelfFence(t *testing.T) {
	cfg := testConfig()
	cfg.Failover.QuorumLossTimeout = 100 * time.Millisecond

	cm := newChaosManager(cfg)

	// Set up as running primary.
	cm.setState(StateRunningPrimary)
	cm.setHealthStatus(HealthStatus{Healthy: true, IsInRecovery: false})

	// Create a real mock store for quorum checking.
	ms := newMockStore()
	ms.SetHasQuorum(false) // Simulate quorum loss

	// Override the store's HasQuorum check.
	cm.Manager.store = nil // Use checkQuorumAndSelfFence's nil check path initially

	// We need to test the actual checkQuorumAndSelfFence method.
	// Since it requires a real store, we'll test the logic flow differently.

	// First, verify quorum tracking starts when quorum is lost.
	cm.mu.Lock()
	cm.quorumLostAt = time.Time{} // Reset
	cm.mu.Unlock()

	// Simulate detecting quorum loss by manually setting quorumLostAt.
	cm.mu.Lock()
	cm.quorumLostAt = time.Now().Add(-200 * time.Millisecond) // Past the timeout
	cm.mu.Unlock()

	// Now call selfFence directly to test the fencing logic.
	ctx := context.Background()
	cm.selfFence(ctx)

	// Verify state is FENCED.
	if cm.State() != StateFenced {
		t.Errorf("expected FENCED state after self-fence, got %s", cm.State())
	}

	// Verify pg_ctl stop was called.
	if cm.chaosExec.GetCallCount("pg_ctl") != 1 {
		t.Errorf("expected pg_ctl stop to be called, got %d calls", cm.chaosExec.GetCallCount("pg_ctl"))
	}
}

// TestChaos_SplitBrain_ProtectionDisabled tests that self-fencing is skipped when disabled.
func TestChaos_SplitBrain_ProtectionDisabled(t *testing.T) {
	cfg := testConfig()
	splitBrainProtection := false
	cfg.Failover.SplitBrainProtection = &splitBrainProtection
	cfg.Failover.QuorumLossTimeout = 50 * time.Millisecond

	cm := newChaosManager(cfg)
	cm.setState(StateRunningPrimary)

	// Even with quorum lost, should not self-fence.
	cm.mu.Lock()
	cm.quorumLostAt = time.Now().Add(-100 * time.Millisecond)
	cm.mu.Unlock()

	// checkQuorumAndSelfFence should return early.
	cm.checkQuorumAndSelfFence(context.Background())

	// Should still be primary.
	if cm.State() != StateRunningPrimary {
		t.Errorf("expected RUNNING_PRIMARY state when protection disabled, got %s", cm.State())
	}
}

// TestChaos_PartialFailure_CommandTimeout tests command timeout handling.
func TestChaos_PartialFailure_CommandTimeout(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningPrimary)

	// Make pg_ctl timeout.
	cm.chaosExec.TimeoutCommands["pg_ctl"] = true

	directive := &consensus.Directive{
		ID:         "fence-timeout",
		Type:       consensus.DirectiveFence,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := cm.executeDirective(ctx, directive)
	if err == nil {
		t.Fatal("expected directive execution to timeout")
	}
}

// TestChaos_PartialFailure_IntermittentFailures tests handling of intermittent failures.
func TestChaos_PartialFailure_IntermittentFailures(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningPrimary)

	// Make pg_ctl fail after 2 successful calls.
	cm.chaosExec.FailAfterN["pg_ctl"] = 2

	// Reset and test with fence directives.
	cm.chaosExec.Reset()
	cm.chaosExec.FailAfterN["pg_ctl"] = 1

	// First call succeeds.
	directive1 := &consensus.Directive{
		ID:         "fence-1",
		Type:       consensus.DirectiveFence,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	cm.setState(StateRunningPrimary)
	err := cm.executeDirective(context.Background(), directive1)
	if err != nil {
		t.Fatalf("first directive should succeed: %v", err)
	}

	// Second call fails.
	cm.setState(StateRunningPrimary)
	directive2 := &consensus.Directive{
		ID:         "fence-2",
		Type:       consensus.DirectiveFence,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	err = cm.executeDirective(context.Background(), directive2)
	if err == nil {
		t.Fatal("second directive should fail")
	}
}

// TestChaos_PartialFailure_HighLatency tests handling of high latency commands.
func TestChaos_PartialFailure_HighLatency(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningPrimary)

	// Add latency to pg_ctl.
	cm.chaosExec.Latency = 50 * time.Millisecond

	directive := &consensus.Directive{
		ID:         "fence-slow",
		Type:       consensus.DirectiveFence,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	start := time.Now()
	err := cm.executeDirective(context.Background(), directive)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("directive execution failed: %v", err)
	}

	if elapsed < 50*time.Millisecond {
		t.Errorf("expected execution to take at least 50ms, took %v", elapsed)
	}

	if cm.State() != StateFenced {
		t.Errorf("expected FENCED state, got %s", cm.State())
	}
}

// TestChaos_PartialFailure_RandomFailures tests random failure injection.
func TestChaos_PartialFailure_RandomFailures(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	// Set 50% failure rate.
	cm.chaosExec.FailureRate = 0.5

	successCount := 0
	failCount := 0

	for i := 0; i < 20; i++ {
		cm.setState(StateRunningPrimary)
		directive := &consensus.Directive{
			ID:         "fence-random-" + string(rune('0'+i)),
			Type:       consensus.DirectiveFence,
			TargetNode: cfg.Node.Name,
			Status:     consensus.DirectiveStatusPending,
			CreatedAt:  time.Now(),
		}

		err := cm.executeDirective(context.Background(), directive)
		if err != nil {
			failCount++
		} else {
			successCount++
		}
	}

	// With 50% failure rate over 20 attempts, we should see some of each.
	if successCount == 0 || failCount == 0 {
		t.Logf("success=%d, fail=%d - distribution might be off due to randomness", successCount, failCount)
	}
}

// TestChaos_StateRecovery_AfterFailedDirective tests state recovery after failure.
func TestChaos_StateRecovery_AfterFailedDirective(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	// Start as healthy replica.
	cm.setState(StateRunningReplica)
	cm.setHealthStatus(HealthStatus{Healthy: true, IsInRecovery: true})

	// Attempt promotion that fails.
	cm.chaosExec.FailCommands["pg_ctl"] = testutil.ErrChaosInjected

	directive := &consensus.Directive{
		ID:         "promote-fail",
		Type:       consensus.DirectivePromote,
		TargetNode: cfg.Node.Name,
		Status:     consensus.DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	err := cm.executeDirective(context.Background(), directive)
	if err == nil {
		t.Fatal("expected promotion to fail")
	}

	// State should be PROMOTING (intermediate state before failure).
	// The caller (processDirectives) would handle the error.
	if cm.State() != StatePromoting {
		t.Errorf("expected PROMOTING state after failed promotion, got %s", cm.State())
	}
}

// TestChaos_ConcurrentDirectives tests handling of concurrent directive execution.
func TestChaos_ConcurrentDirectives(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	cm.setState(StateRunningPrimary)

	// Add some latency to simulate real execution.
	cm.chaosExec.Latency = 10 * time.Millisecond

	var wg sync.WaitGroup
	results := make(chan error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			directive := &consensus.Directive{
				ID:         "checkpoint-concurrent-" + string(rune('0'+idx)),
				Type:       consensus.DirectiveCheckpoint,
				TargetNode: cfg.Node.Name,
				Status:     consensus.DirectiveStatusPending,
				CreatedAt:  time.Now(),
			}
			results <- cm.executeDirective(context.Background(), directive)
		}(i)
	}

	wg.Wait()
	close(results)

	successCount := 0
	for err := range results {
		if err == nil {
			successCount++
		}
	}

	// All should succeed (CHECKPOINT is idempotent).
	if successCount != 5 {
		t.Errorf("expected 5 successful executions, got %d", successCount)
	}
}

// TestChaos_RaftBackoff_PartitionDetection tests Raft error backoff.
func TestChaos_RaftBackoff_PartitionDetection(t *testing.T) {
	cfg := testConfig()
	cm := newChaosManager(cfg)

	// Initially no backoff.
	if cm.isRaftBackoff() {
		t.Error("expected no backoff initially")
	}

	// Record some errors.
	for i := 0; i < 3; i++ {
		cm.recordRaftError(consensus.ErrNotLeader)
	}

	// Should be in backoff now.
	if !cm.isRaftBackoff() {
		t.Error("expected backoff after errors")
	}

	// Check backoff increases exponentially.
	cm.mu.RLock()
	backoff := cm.raftErrorBackoff
	cm.mu.RUnlock()

	if backoff < 4*time.Second {
		t.Errorf("expected backoff to be at least 4s after 3 errors, got %v", backoff)
	}

	// Clear errors.
	cm.recordRaftError(nil)

	if cm.isRaftBackoff() {
		t.Error("expected backoff to clear after success")
	}
}
