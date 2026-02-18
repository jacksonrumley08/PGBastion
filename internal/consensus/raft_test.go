package consensus

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewRaftNode_Bootstrap(t *testing.T) {
	dir := t.TempDir()
	fsm := NewClusterFSM()

	r, teardown, err := NewRaftNode("node1", "127.0.0.1", 0, dir, true, DefaultRaftOptions(), fsm, testLogger())
	if err != nil {
		t.Fatalf("NewRaftNode: %v", err)
	}
	defer teardown()

	// Wait for leader election.
	time.Sleep(3 * time.Second)

	if r.State().String() != "Leader" {
		t.Errorf("expected Leader state, got %s", r.State())
	}
}

func TestNewRaftNode_NoBootstrapOnExistingLog(t *testing.T) {
	dir := t.TempDir()
	fsm := NewClusterFSM()

	// First: bootstrap.
	r1, teardown1, err := NewRaftNode("node1", "127.0.0.1", 0, dir, true, DefaultRaftOptions(), fsm, testLogger())
	if err != nil {
		t.Fatalf("first NewRaftNode: %v", err)
	}
	time.Sleep(3 * time.Second)
	if r1.State().String() != "Leader" {
		t.Fatalf("expected leader after bootstrap, got %s", r1.State())
	}
	teardown1()

	// Second: re-open with bootstrap=true — should NOT re-bootstrap (log not empty).
	fsm2 := NewClusterFSM()
	r2, teardown2, err := NewRaftNode("node1", "127.0.0.1", 0, dir, true, DefaultRaftOptions(), fsm2, testLogger())
	if err != nil {
		t.Fatalf("second NewRaftNode: %v", err)
	}
	defer teardown2()

	time.Sleep(3 * time.Second)
	if r2.State().String() != "Leader" {
		t.Errorf("expected Leader after reopen, got %s", r2.State())
	}
}

func TestNewRaftNode_StoreIntegration(t *testing.T) {
	dir := t.TempDir()
	fsm := NewClusterFSM()

	r, teardown, err := NewRaftNode("node1", "127.0.0.1", 0, dir, true, DefaultRaftOptions(), fsm, testLogger())
	if err != nil {
		t.Fatalf("NewRaftNode: %v", err)
	}
	defer teardown()

	time.Sleep(3 * time.Second)

	store := NewStore(r, fsm)

	if !store.IsLeader() {
		t.Fatal("expected to be leader")
	}

	// Write primary.
	if err := store.SetPrimary(NodeInfo{Name: "pg1", Host: "10.0.1.1", Port: 5432, State: "running"}); err != nil {
		t.Fatalf("SetPrimary: %v", err)
	}

	// Write replica.
	if err := store.SetReplica(NodeInfo{Name: "pg2", Host: "10.0.1.2", Port: 5432, State: "streaming"}); err != nil {
		t.Fatalf("SetReplica: %v", err)
	}

	// Read back from FSM.
	p := fsm.GetPrimary()
	if p == nil || p.Name != "pg1" {
		t.Error("expected primary=pg1")
	}

	replicas := fsm.GetReplicas()
	if len(replicas) != 1 || replicas[0].Name != "pg2" {
		t.Error("expected 1 replica=pg2")
	}

	// Test RaftStateAdapter.
	adapter := NewRaftStateAdapter(fsm)
	pn := adapter.GetPrimary()
	if pn == nil || pn.Name != "pg1" {
		t.Error("adapter: expected primary=pg1")
	}
	rn := adapter.GetReplicas()
	if len(rn) != 1 || rn[0].Name != "pg2" {
		t.Error("adapter: expected 1 replica=pg2")
	}
}

func TestNewRaftNode_StoreNotLeader(t *testing.T) {
	// Create a raft node but don't bootstrap — it will be a follower/candidate.
	dir := t.TempDir()
	fsm := NewClusterFSM()

	r, teardown, err := NewRaftNode("node1", "127.0.0.1", 0, dir, false, DefaultRaftOptions(), fsm, testLogger())
	if err != nil {
		t.Fatalf("NewRaftNode: %v", err)
	}
	defer teardown()

	store := NewStore(r, fsm)

	err = store.SetPrimary(NodeInfo{Name: "pg1", Host: "10.0.1.1", Port: 5432})
	if err == nil {
		t.Fatal("expected ErrNotLeader")
	}
	if err.Error() != "not the raft leader" {
		t.Errorf("expected ErrNotLeader, got: %v", err)
	}
}
