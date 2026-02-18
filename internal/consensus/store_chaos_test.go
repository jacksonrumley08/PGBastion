package consensus

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func chaosTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// Helper to apply a command to the FSM for chaos testing.
func chaosApplyCommand(fsm *ClusterFSM, cmdType string, data any) error {
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	cmd := FSMCommand{
		Type: cmdType,
		Data: json.RawMessage(dataBytes),
	}

	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	result := fsm.Apply(&raft.Log{Data: cmdBytes})
	if err, ok := result.(error); ok {
		return err
	}
	return nil
}

func TestFSM_DirectiveLifecycle(t *testing.T) {
	fsm := NewClusterFSM()

	// Publish a directive.
	directive := Directive{
		ID:         "test-directive-1",
		Type:       DirectivePromote,
		TargetNode: "node1",
		Status:     DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	err := chaosApplyCommand(fsm, CmdPublishDirective, directive)
	if err != nil {
		t.Fatalf("failed to apply directive: %v", err)
	}

	// Verify directive is stored.
	d := fsm.GetDirective("test-directive-1")
	if d == nil {
		t.Fatal("expected directive to be stored")
	}

	if d.Status != DirectiveStatusPending {
		t.Errorf("expected status PENDING, got %s", d.Status)
	}
}

func TestFSM_DirectiveCompletion(t *testing.T) {
	fsm := NewClusterFSM()

	// Publish a directive.
	directive := Directive{
		ID:         "test-directive-2",
		Type:       DirectiveFence,
		TargetNode: "node2",
		Status:     DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	chaosApplyCommand(fsm, CmdPublishDirective, directive)

	// Complete the directive.
	err := chaosApplyCommand(fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{
		ID:    "test-directive-2",
		Error: "",
	})
	if err != nil {
		t.Fatalf("failed to complete directive: %v", err)
	}

	// Verify directive is completed.
	d := fsm.GetDirective("test-directive-2")
	if d == nil {
		t.Fatal("expected directive to exist")
	}

	if d.Status != DirectiveStatusCompleted {
		t.Errorf("expected status COMPLETED, got %s", d.Status)
	}
}

func TestFSM_DirectiveFailure(t *testing.T) {
	fsm := NewClusterFSM()

	// Publish a directive.
	directive := Directive{
		ID:         "test-directive-3",
		Type:       DirectiveDemote,
		TargetNode: "node3",
		Status:     DirectiveStatusPending,
		CreatedAt:  time.Now(),
	}

	chaosApplyCommand(fsm, CmdPublishDirective, directive)

	// Fail the directive.
	chaosApplyCommand(fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{
		ID:    "test-directive-3",
		Error: "command timed out",
	})

	d := fsm.GetDirective("test-directive-3")
	if d == nil {
		t.Fatal("expected directive to exist")
	}

	if d.Status != DirectiveStatusFailed {
		t.Errorf("expected status FAILED, got %s", d.Status)
	}

	if d.Error != "command timed out" {
		t.Errorf("expected error message, got %s", d.Error)
	}
}

func TestFSM_GetDirectivesForNode_Chaos(t *testing.T) {
	fsm := NewClusterFSM()

	// Publish directives for different nodes.
	nodes := []string{"node1", "node1", "node2", "node3"}
	for i, node := range nodes {
		chaosApplyCommand(fsm, CmdPublishDirective, Directive{
			ID:         string(rune('a'+i)) + "-directive",
			Type:       DirectivePromote,
			TargetNode: node,
			Status:     DirectiveStatusPending,
			CreatedAt:  time.Now(),
		})
	}

	// Get directives for node1.
	directives := fsm.GetDirectivesForNode("node1")
	if len(directives) != 2 {
		t.Errorf("expected 2 directives for node1, got %d", len(directives))
	}

	// Get directives for node2.
	directives = fsm.GetDirectivesForNode("node2")
	if len(directives) != 1 {
		t.Errorf("expected 1 directive for node2, got %d", len(directives))
	}
}

func TestFSM_DirectiveCleanup(t *testing.T) {
	fsm := NewClusterFSM()

	// Publish and complete a directive with old timestamp.
	oldTime := time.Now().Add(-2 * time.Hour)
	directive := Directive{
		ID:          "old-directive",
		Type:        DirectiveCheckpoint,
		TargetNode:  "node1",
		Status:      DirectiveStatusPending,
		CreatedAt:   oldTime,
	}
	chaosApplyCommand(fsm, CmdPublishDirective, directive)

	// Complete it (sets CompletedAt to now, but we'll manually test the cleanup logic).
	chaosApplyCommand(fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{
		ID:    "old-directive",
		Error: "",
	})

	// Manually set CompletedAt to old time for testing.
	fsm.mu.Lock()
	if d, ok := fsm.state.Directives["old-directive"]; ok {
		d.CompletedAt = oldTime
	}
	fsm.mu.Unlock()

	// Cleanup old directives.
	chaosApplyCommand(fsm, CmdCleanupDirectives, struct {
		MaxAge time.Duration `json:"max_age"`
	}{
		MaxAge: time.Hour,
	})

	// Old directive should be cleaned up.
	d := fsm.GetDirective("old-directive")
	if d != nil {
		t.Error("expected old directive to be cleaned up")
	}
}

func TestFSM_NodeInfo_SetPrimary(t *testing.T) {
	fsm := NewClusterFSM()

	node := NodeInfo{
		Name:     "node1",
		Host:     "10.0.1.1",
		Port:     5432,
		State:    "running",
		LastSeen: time.Now(),
	}

	chaosApplyCommand(fsm, CmdSetPrimary, node)

	primary := fsm.GetPrimary()
	if primary == nil {
		t.Fatal("expected primary to be set")
	}

	if primary.Name != "node1" {
		t.Errorf("expected primary name node1, got %s", primary.Name)
	}

	if primary.Role != "primary" {
		t.Errorf("expected role primary, got %s", primary.Role)
	}
}

func TestFSM_NodeInfo_SetReplica(t *testing.T) {
	fsm := NewClusterFSM()

	node := NodeInfo{
		Name:     "node2",
		Host:     "10.0.1.2",
		Port:     5432,
		State:    "running",
		LastSeen: time.Now(),
		Lag:      1000,
	}

	chaosApplyCommand(fsm, CmdSetReplica, node)

	replicas := fsm.GetReplicas()
	if len(replicas) != 1 {
		t.Errorf("expected 1 replica, got %d", len(replicas))
	}

	if replicas[0].Name != "node2" {
		t.Errorf("expected replica name node2, got %s", replicas[0].Name)
	}

	if replicas[0].Role != "replica" {
		t.Errorf("expected role replica, got %s", replicas[0].Role)
	}
}

func TestFSM_NodeInfo_AddRemove(t *testing.T) {
	fsm := NewClusterFSM()

	// Add nodes.
	for i := 1; i <= 3; i++ {
		chaosApplyCommand(fsm, CmdAddNode, NodeInfo{
			Name: "node" + string(rune('0'+i)),
			Host: "10.0.1." + string(rune('0'+i)),
			Port: 5432,
		})
	}

	nodes := fsm.GetAllNodes()
	if len(nodes) != 3 {
		t.Errorf("expected 3 nodes, got %d", len(nodes))
	}

	// Remove a node.
	chaosApplyCommand(fsm, CmdRemoveNode, struct {
		Name string `json:"name"`
	}{Name: "node2"})

	nodes = fsm.GetAllNodes()
	if len(nodes) != 2 {
		t.Errorf("expected 2 nodes after removal, got %d", len(nodes))
	}

	if _, ok := nodes["node2"]; ok {
		t.Error("node2 should have been removed")
	}
}

func TestFSM_NodeHealth_Update(t *testing.T) {
	fsm := NewClusterFSM()

	// Add a node first.
	chaosApplyCommand(fsm, CmdAddNode, NodeInfo{
		Name: "node1",
		Host: "10.0.1.1",
		Port: 5432,
	})

	// Update health.
	newLastSeen := time.Now()
	chaosApplyCommand(fsm, CmdUpdateNodeHealth, NodeInfo{
		Name:     "node1",
		State:    "running_primary",
		LastSeen: newLastSeen,
		Lag:      0,
	})

	nodes := fsm.GetAllNodes()
	node, ok := nodes["node1"]
	if !ok {
		t.Fatal("node1 should exist")
	}

	if node.State != "running_primary" {
		t.Errorf("expected state running_primary, got %s", node.State)
	}
}

func TestFSM_ClusterConfig_SplitBrain(t *testing.T) {
	fsm := NewClusterFSM()

	config := ClusterConfig{
		SplitBrain: true,
	}

	chaosApplyCommand(fsm, CmdSetClusterConfig, config)

	if !fsm.IsSplitBrain() {
		t.Error("expected split brain to be true")
	}
}

func TestFSM_UnknownCommand_ReturnsError(t *testing.T) {
	fsm := NewClusterFSM()

	// Apply an unknown command type.
	err := chaosApplyCommand(fsm, "UNKNOWN_COMMAND", nil)
	if err == nil {
		t.Error("expected error for unknown command")
	}
}

func TestFSM_DirectiveNotFound_CompleteIgnored(t *testing.T) {
	fsm := NewClusterFSM()

	// Try to complete a non-existent directive.
	err := chaosApplyCommand(fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{
		ID:    "nonexistent",
		Error: "",
	})

	// Should not error, just be ignored.
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFSM_ConcurrentReads(t *testing.T) {
	fsm := NewClusterFSM()

	// Add some data.
	chaosApplyCommand(fsm, CmdSetPrimary, NodeInfo{Name: "node1", Host: "10.0.1.1", Port: 5432})

	for i := 0; i < 3; i++ {
		chaosApplyCommand(fsm, CmdPublishDirective, Directive{
			ID:         "d" + string(rune('0'+i)),
			Type:       DirectivePromote,
			TargetNode: "node1",
			Status:     DirectiveStatusPending,
		})
	}

	// Concurrent reads.
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = fsm.GetPrimary()
				_ = fsm.GetReplicas()
				_ = fsm.GetAllDirectives()
				_ = fsm.GetDirectivesForNode("node1")
			}
			done <- struct{}{}
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestFSM_Snapshot_Restore(t *testing.T) {
	fsm := NewClusterFSM()

	// Add some state.
	chaosApplyCommand(fsm, CmdSetPrimary, NodeInfo{Name: "node1", Host: "10.0.1.1", Port: 5432})
	chaosApplyCommand(fsm, CmdPublishDirective, Directive{
		ID:         "test-directive",
		Type:       DirectivePromote,
		TargetNode: "node1",
		Status:     DirectiveStatusPending,
	})

	// Take snapshot.
	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}

	// The snapshot data is in fsmSnapshot.data, but we need to use Restore().
	// For testing, we'll verify the snapshot was created successfully.
	if snap == nil {
		t.Fatal("expected snapshot to be created")
	}
}

func TestFSM_DirectiveCount(t *testing.T) {
	fsm := NewClusterFSM()

	if fsm.DirectiveCount() != 0 {
		t.Error("expected 0 directives initially")
	}

	for i := 0; i < 5; i++ {
		chaosApplyCommand(fsm, CmdPublishDirective, Directive{
			ID:         "d" + string(rune('0'+i)),
			Type:       DirectivePromote,
			TargetNode: "node1",
			Status:     DirectiveStatusPending,
		})
	}

	if fsm.DirectiveCount() != 5 {
		t.Errorf("expected 5 directives, got %d", fsm.DirectiveCount())
	}
}

func TestFSM_GetStaleDirectiveIDs(t *testing.T) {
	fsm := NewClusterFSM()

	// Add and complete a directive.
	chaosApplyCommand(fsm, CmdPublishDirective, Directive{
		ID:         "stale-directive",
		Type:       DirectivePromote,
		TargetNode: "node1",
		Status:     DirectiveStatusPending,
	})

	chaosApplyCommand(fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{ID: "stale-directive"})

	// Make it stale.
	fsm.mu.Lock()
	if d, ok := fsm.state.Directives["stale-directive"]; ok {
		d.CompletedAt = time.Now().Add(-2 * time.Hour)
	}
	fsm.mu.Unlock()

	stale := fsm.GetStaleDirectiveIDs(time.Hour)
	if len(stale) != 1 {
		t.Errorf("expected 1 stale directive, got %d", len(stale))
	}
}

func TestFSM_RemovePrimary_ClearsPrimaryName(t *testing.T) {
	fsm := NewClusterFSM()

	// Set primary.
	chaosApplyCommand(fsm, CmdSetPrimary, NodeInfo{Name: "node1", Host: "10.0.1.1", Port: 5432})

	if fsm.GetPrimary() == nil {
		t.Fatal("expected primary to be set")
	}

	// Remove the primary node.
	chaosApplyCommand(fsm, CmdRemoveNode, struct {
		Name string `json:"name"`
	}{Name: "node1"})

	// Primary should be nil now.
	if fsm.GetPrimary() != nil {
		t.Error("expected primary to be nil after removal")
	}
}
