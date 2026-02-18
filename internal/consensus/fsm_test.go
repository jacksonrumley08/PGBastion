package consensus

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func applyCommand(t *testing.T, fsm *ClusterFSM, cmdType string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	cmd := FSMCommand{Type: cmdType, Data: json.RawMessage(raw)}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	resp := fsm.Apply(&raft.Log{Data: cmdBytes})
	if err, ok := resp.(error); ok && err != nil {
		t.Fatalf("apply %s: %v", cmdType, err)
	}
}

func TestFSM_SetPrimary(t *testing.T) {
	fsm := NewClusterFSM()
	applyCommand(t, fsm, CmdSetPrimary, NodeInfo{Name: "node1", Host: "10.0.1.1", Port: 5432, State: "running"})

	p := fsm.GetPrimary()
	if p == nil {
		t.Fatal("expected primary")
	}
	if p.Name != "node1" {
		t.Errorf("expected name=node1, got %s", p.Name)
	}
	if p.Role != "primary" {
		t.Errorf("expected role=primary, got %s", p.Role)
	}
}

func TestFSM_SetReplica(t *testing.T) {
	fsm := NewClusterFSM()
	applyCommand(t, fsm, CmdSetReplica, NodeInfo{Name: "node2", Host: "10.0.1.2", Port: 5432, State: "streaming"})

	replicas := fsm.GetReplicas()
	if len(replicas) != 1 {
		t.Fatalf("expected 1 replica, got %d", len(replicas))
	}
	if replicas[0].Name != "node2" {
		t.Errorf("expected name=node2, got %s", replicas[0].Name)
	}
	if replicas[0].Role != "replica" {
		t.Errorf("expected role=replica, got %s", replicas[0].Role)
	}
}

func TestFSM_AddAndRemoveNode(t *testing.T) {
	fsm := NewClusterFSM()
	applyCommand(t, fsm, CmdAddNode, NodeInfo{Name: "node3", Host: "10.0.1.3", Port: 5432})

	nodes := fsm.GetAllNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	applyCommand(t, fsm, CmdRemoveNode, struct{ Name string `json:"name"` }{Name: "node3"})
	nodes = fsm.GetAllNodes()
	if len(nodes) != 0 {
		t.Fatalf("expected 0 nodes after removal, got %d", len(nodes))
	}
}

func TestFSM_RemoveNode_ClearsPrimary(t *testing.T) {
	fsm := NewClusterFSM()
	applyCommand(t, fsm, CmdSetPrimary, NodeInfo{Name: "node1", Host: "10.0.1.1", Port: 5432})
	applyCommand(t, fsm, CmdRemoveNode, struct{ Name string `json:"name"` }{Name: "node1"})

	if fsm.GetPrimary() != nil {
		t.Error("expected primary to be cleared after removal")
	}
}

func TestFSM_UpdateNodeHealth(t *testing.T) {
	fsm := NewClusterFSM()
	applyCommand(t, fsm, CmdAddNode, NodeInfo{Name: "node1", Host: "10.0.1.1", Port: 5432, State: "running"})

	now := time.Now()
	applyCommand(t, fsm, CmdUpdateNodeHealth, NodeInfo{Name: "node1", State: "stopped", Lag: 100, LastSeen: now})

	nodes := fsm.GetAllNodes()
	if nodes["node1"].State != "stopped" {
		t.Errorf("expected state=stopped, got %s", nodes["node1"].State)
	}
	if nodes["node1"].Lag != 100 {
		t.Errorf("expected lag=100, got %d", nodes["node1"].Lag)
	}
}

func TestFSM_SetClusterConfig(t *testing.T) {
	fsm := NewClusterFSM()
	applyCommand(t, fsm, CmdSetClusterConfig, ClusterConfig{SplitBrain: true})

	if !fsm.IsSplitBrain() {
		t.Error("expected split_brain=true")
	}

	applyCommand(t, fsm, CmdSetClusterConfig, ClusterConfig{SplitBrain: false})
	if fsm.IsSplitBrain() {
		t.Error("expected split_brain=false")
	}
}

func TestFSM_SnapshotRestore(t *testing.T) {
	fsm := NewClusterFSM()
	applyCommand(t, fsm, CmdSetPrimary, NodeInfo{Name: "node1", Host: "10.0.1.1", Port: 5432, State: "running"})
	applyCommand(t, fsm, CmdSetReplica, NodeInfo{Name: "node2", Host: "10.0.1.2", Port: 5432, State: "streaming"})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("persist: %v", err)
	}

	fsm2 := NewClusterFSM()
	if err := fsm2.Restore(io.NopCloser(bytes.NewReader(sink.data))); err != nil {
		t.Fatalf("restore: %v", err)
	}

	p := fsm2.GetPrimary()
	if p == nil || p.Name != "node1" {
		t.Error("expected restored primary=node1")
	}
	if len(fsm2.GetReplicas()) != 1 {
		t.Error("expected 1 restored replica")
	}
}

func TestFSM_UnknownCommand(t *testing.T) {
	fsm := NewClusterFSM()
	cmd := FSMCommand{Type: "INVALID", Data: json.RawMessage(`{}`)}
	cmdBytes, _ := json.Marshal(cmd)
	resp := fsm.Apply(&raft.Log{Data: cmdBytes})
	if resp == nil {
		t.Fatal("expected error for unknown command")
	}
	if _, ok := resp.(error); !ok {
		t.Fatal("expected error type response")
	}
}

// memSink is a simple in-memory raft.SnapshotSink for testing.
type memSink struct {
	data []byte
}

func (s *memSink) Write(p []byte) (int, error) {
	s.data = append(s.data, p...)
	return len(p), nil
}

func (s *memSink) Close() error  { return nil }
func (s *memSink) ID() string    { return "test" }
func (s *memSink) Cancel() error { return nil }

func TestFSM_PublishDirective(t *testing.T) {
	fsm := NewClusterFSM()
	now := time.Now()
	applyCommand(t, fsm, CmdPublishDirective, Directive{
		ID:         "fence-node1-123",
		Type:       DirectiveFence,
		TargetNode: "node1",
		Status:     DirectiveStatusPending,
		CreatedAt:  now,
	})

	d := fsm.GetDirective("fence-node1-123")
	if d == nil {
		t.Fatal("expected directive to exist")
	}
	if d.Type != DirectiveFence {
		t.Errorf("expected type=FENCE, got %s", d.Type)
	}
	if d.TargetNode != "node1" {
		t.Errorf("expected target=node1, got %s", d.TargetNode)
	}
	if d.Status != DirectiveStatusPending {
		t.Errorf("expected status=pending, got %s", d.Status)
	}
}

func TestFSM_CompleteDirective(t *testing.T) {
	fsm := NewClusterFSM()
	now := time.Now()
	applyCommand(t, fsm, CmdPublishDirective, Directive{
		ID:         "promote-node2-456",
		Type:       DirectivePromote,
		TargetNode: "node2",
		Status:     DirectiveStatusPending,
		CreatedAt:  now,
	})

	applyCommand(t, fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{ID: "promote-node2-456", Error: ""})

	d := fsm.GetDirective("promote-node2-456")
	if d == nil {
		t.Fatal("expected directive to exist")
	}
	if d.Status != DirectiveStatusCompleted {
		t.Errorf("expected status=completed, got %s", d.Status)
	}
	if d.CompletedAt.IsZero() {
		t.Error("expected CompletedAt to be set")
	}
}

func TestFSM_CompleteDirective_WithError(t *testing.T) {
	fsm := NewClusterFSM()
	now := time.Now()
	applyCommand(t, fsm, CmdPublishDirective, Directive{
		ID:         "fence-node3-789",
		Type:       DirectiveFence,
		TargetNode: "node3",
		Status:     DirectiveStatusPending,
		CreatedAt:  now,
	})

	applyCommand(t, fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{ID: "fence-node3-789", Error: "pg_ctl stop failed"})

	d := fsm.GetDirective("fence-node3-789")
	if d == nil {
		t.Fatal("expected directive to exist")
	}
	if d.Status != DirectiveStatusFailed {
		t.Errorf("expected status=failed, got %s", d.Status)
	}
	if d.Error != "pg_ctl stop failed" {
		t.Errorf("expected error message, got %s", d.Error)
	}
}

func TestFSM_GetDirectivesForNode(t *testing.T) {
	fsm := NewClusterFSM()
	now := time.Now()

	// Add directives for different nodes.
	applyCommand(t, fsm, CmdPublishDirective, Directive{
		ID:         "fence-node1-1",
		Type:       DirectiveFence,
		TargetNode: "node1",
		Status:     DirectiveStatusPending,
		CreatedAt:  now,
	})
	applyCommand(t, fsm, CmdPublishDirective, Directive{
		ID:         "promote-node2-1",
		Type:       DirectivePromote,
		TargetNode: "node2",
		Status:     DirectiveStatusPending,
		CreatedAt:  now,
	})
	applyCommand(t, fsm, CmdPublishDirective, Directive{
		ID:         "checkpoint-node1-1",
		Type:       DirectiveCheckpoint,
		TargetNode: "node1",
		Status:     DirectiveStatusPending,
		CreatedAt:  now,
	})

	// Complete one of node1's directives.
	applyCommand(t, fsm, CmdCompleteDirective, struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{ID: "fence-node1-1"})

	node1Directives := fsm.GetDirectivesForNode("node1")
	if len(node1Directives) != 1 {
		t.Errorf("expected 1 pending directive for node1, got %d", len(node1Directives))
	}
	if node1Directives[0].ID != "checkpoint-node1-1" {
		t.Errorf("expected checkpoint directive, got %s", node1Directives[0].ID)
	}

	node2Directives := fsm.GetDirectivesForNode("node2")
	if len(node2Directives) != 1 {
		t.Errorf("expected 1 pending directive for node2, got %d", len(node2Directives))
	}
}
