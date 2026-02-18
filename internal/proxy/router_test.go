package proxy

import (
	"log/slog"
	"os"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// mockClusterState implements ClusterStateSource for testing.
type mockClusterState struct {
	primary    *NodeState
	replicas   []*NodeState
	splitBrain bool
}

func (m *mockClusterState) GetPrimary() *NodeState    { return m.primary }
func (m *mockClusterState) GetReplicas() []*NodeState { return m.replicas }
func (m *mockClusterState) IsSplitBrain() bool        { return m.splitBrain }

func stateWithPrimary(name, host string, port int) *mockClusterState {
	return &mockClusterState{
		primary: &NodeState{Name: name, Host: host, Port: port},
	}
}

func stateWithReplicas(primary *NodeState, replicas ...*NodeState) *mockClusterState {
	return &mockClusterState{
		primary:  primary,
		replicas: replicas,
	}
}

func TestRouter_PrimaryAddr(t *testing.T) {
	state := stateWithPrimary("node1", "10.0.1.1", 5432)
	r := NewRouter(state, testLogger())

	addr := r.PrimaryAddr()
	if addr != "10.0.1.1:5432" {
		t.Errorf("expected 10.0.1.1:5432, got %s", addr)
	}
}

func TestRouter_PrimaryAddr_NoPrimary(t *testing.T) {
	state := &mockClusterState{}
	r := NewRouter(state, testLogger())

	if addr := r.PrimaryAddr(); addr != "" {
		t.Errorf("expected empty string, got %s", addr)
	}
}

func TestRouter_PrimaryAddr_SplitBrain(t *testing.T) {
	state := &mockClusterState{
		splitBrain: true,
		primary:    &NodeState{Name: "node1", Host: "10.0.1.1", Port: 5432},
	}
	r := NewRouter(state, testLogger())

	if addr := r.PrimaryAddr(); addr != "" {
		t.Errorf("expected empty string during split-brain, got %s", addr)
	}
}

func TestRouter_ReplicaAddr_RoundRobin(t *testing.T) {
	state := stateWithReplicas(
		&NodeState{Name: "node1", Host: "10.0.1.1", Port: 5432},
		&NodeState{Name: "node2", Host: "10.0.1.2", Port: 5432},
		&NodeState{Name: "node3", Host: "10.0.1.3", Port: 5432},
	)
	r := NewRouter(state, testLogger())

	// Should return replicas (order may vary but should cycle).
	seen := make(map[string]int)
	for i := 0; i < 6; i++ {
		addr := r.ReplicaAddr()
		if addr == "" {
			t.Fatal("unexpected empty replica addr")
		}
		seen[addr]++
	}

	if len(seen) < 2 {
		t.Errorf("expected at least 2 different replicas in round-robin, got %d", len(seen))
	}
}

func TestRouter_ReplicaAddr_NoReplicas_FallsToPrimary(t *testing.T) {
	state := stateWithPrimary("node1", "10.0.1.1", 5432)
	r := NewRouter(state, testLogger())

	addr := r.ReplicaAddr()
	if addr != "10.0.1.1:5432" {
		t.Errorf("expected fallback to primary 10.0.1.1:5432, got %s", addr)
	}
}

func TestRouter_ReplicaAddr_SplitBrain(t *testing.T) {
	state := &mockClusterState{
		splitBrain: true,
		replicas: []*NodeState{
			{Name: "node2", Host: "10.0.1.2", Port: 5432},
		},
	}
	r := NewRouter(state, testLogger())

	if addr := r.ReplicaAddr(); addr != "" {
		t.Errorf("expected empty string during split-brain, got %s", addr)
	}
}

func TestRouter_ReplicaAddr_EmptyState(t *testing.T) {
	state := &mockClusterState{}
	r := NewRouter(state, testLogger())

	if addr := r.ReplicaAddr(); addr != "" {
		t.Errorf("expected empty string with no replicas or primary, got %s", addr)
	}
}

func TestRouter_RoutingTable(t *testing.T) {
	state := stateWithReplicas(
		&NodeState{Name: "node1", Host: "10.0.1.1", Port: 5432},
		&NodeState{Name: "node2", Host: "10.0.1.2", Port: 5432},
	)
	r := NewRouter(state, testLogger())

	table := r.RoutingTable()
	if table.Primary != "10.0.1.1:5432" {
		t.Errorf("expected primary=10.0.1.1:5432, got %s", table.Primary)
	}
	if len(table.Replicas) != 1 {
		t.Fatalf("expected 1 replica, got %d", len(table.Replicas))
	}
	if table.Replicas[0] != "10.0.1.2:5432" {
		t.Errorf("expected replica=10.0.1.2:5432, got %s", table.Replicas[0])
	}
	if table.SplitBrain {
		t.Error("should not be split-brain")
	}
}

func TestRouter_RoutingTable_SplitBrain(t *testing.T) {
	state := &mockClusterState{splitBrain: true}
	r := NewRouter(state, testLogger())

	table := r.RoutingTable()
	if !table.SplitBrain {
		t.Error("expected split_brain=true")
	}
}
