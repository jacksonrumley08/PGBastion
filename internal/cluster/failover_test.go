package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/jacksonrumley08/pgbastion/internal/consensus"
)

type mockExecutor struct {
	calls []string
	err   error
}

func (e *mockExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	e.calls = append(e.calls, name)
	return []byte("ok"), e.err
}

func TestFailoverController_ShouldFailover_ConfirmationPeriod(t *testing.T) {
	cfg := testConfig()
	cfg.Failover.ConfirmationPeriod = 100 * time.Millisecond

	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	node := &consensus.NodeInfo{
		Name:     "primary1",
		LastSeen: time.Now().Add(-10 * time.Second),
	}

	// First call starts confirmation timer.
	if fc.ShouldFailover(node) {
		t.Error("should not failover on first detection (confirmation period not elapsed)")
	}

	// Immediately after — still within confirmation period.
	if fc.ShouldFailover(node) {
		t.Error("should not failover within confirmation period")
	}

	// Wait for confirmation period to elapse.
	time.Sleep(150 * time.Millisecond)

	if !fc.ShouldFailover(node) {
		t.Error("should failover after confirmation period elapsed")
	}
}

func TestFailoverController_ShouldFailover_InFlight(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	fc.mu.Lock()
	fc.failoverInFlight = true
	fc.mu.Unlock()

	node := &consensus.NodeInfo{
		Name:     "primary1",
		LastSeen: time.Now().Add(-10 * time.Second),
	}

	if fc.ShouldFailover(node) {
		t.Error("should not failover when another is in flight")
	}
}

func TestFailoverController_SelectBestReplica(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	// Without a store, should fail.
	_, err := fc.selectBestReplica("primary1")
	if err == nil {
		t.Error("expected error without store")
	}
}

func TestFailoverController_IsFailoverInFlight(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	if fc.IsFailoverInFlight() {
		t.Error("expected no failover in flight initially")
	}

	fc.mu.Lock()
	fc.failoverInFlight = true
	fc.mu.Unlock()

	if !fc.IsFailoverInFlight() {
		t.Error("expected failover in flight after setting")
	}
}

func TestFailoverController_ClearFailover(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	fc.mu.Lock()
	fc.failoverInFlight = true
	fc.activeDirectiveIDs = []string{"d1", "d2"}
	fc.failoverStartTime = time.Now()
	fc.confirmationStart = time.Now()
	fc.mu.Unlock()

	fc.clearFailover()

	if fc.IsFailoverInFlight() {
		t.Error("expected failover cleared")
	}
	fc.mu.Lock()
	if len(fc.activeDirectiveIDs) != 0 {
		t.Error("expected active directive IDs cleared")
	}
	if !fc.failoverStartTime.IsZero() {
		t.Error("expected failover start time cleared")
	}
	if !fc.confirmationStart.IsZero() {
		t.Error("expected confirmation start cleared")
	}
	fc.mu.Unlock()
}

func TestFailoverController_ExecuteFailover_NoStore(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	err := fc.ExecuteFailover(context.Background(), "primary1")
	if err == nil {
		t.Error("expected error without store")
	}
}

func TestFailoverController_ExecuteSwitchover_NoStore(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	err := fc.ExecuteSwitchover(context.Background(), "primary1", "replica1")
	if err == nil {
		t.Error("expected error without store")
	}
}

func TestFailoverController_MonitorDirectives_NoStore(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	// Set up a failover in flight.
	fc.mu.Lock()
	fc.failoverInFlight = true
	fc.activeDirectiveIDs = []string{"d1"}
	fc.failoverStartTime = time.Now()
	fc.mu.Unlock()

	// Should return false when store is nil.
	if fc.MonitorDirectives() {
		t.Error("expected false when store is nil")
	}
}

func TestFailoverController_MonitorDirectives_NoFailoverInFlight(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	// No failover in flight.
	if fc.MonitorDirectives() {
		t.Error("expected false when no failover in flight")
	}
}

func TestFailoverController_FailoverStartTimeSet(t *testing.T) {
	cfg := testConfig()
	fc := NewFailoverControllerWithExecutor(cfg, nil, &mockExecutor{}, testLogger())

	// Before failover, start time should be zero.
	fc.mu.Lock()
	if !fc.failoverStartTime.IsZero() {
		t.Error("expected failover start time to be zero initially")
	}
	fc.mu.Unlock()

	// ExecuteFailover will fail due to no store, but should still set failoverInFlight temporarily.
	_ = fc.ExecuteFailover(context.Background(), "primary1")

	// After failed failover, it should be cleared.
	fc.mu.Lock()
	if !fc.failoverStartTime.IsZero() {
		t.Error("expected failover start time to be cleared after failure")
	}
	fc.mu.Unlock()
}
