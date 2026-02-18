package consensus

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// Command types for the FSM.
const (
	CmdSetPrimary         = "SET_PRIMARY"
	CmdSetReplica         = "SET_REPLICA"
	CmdAddNode            = "ADD_NODE"
	CmdRemoveNode         = "REMOVE_NODE"
	CmdUpdateNodeHealth   = "UPDATE_NODE_HEALTH"
	CmdSetClusterConfig   = "SET_CLUSTER_CONFIG"
	CmdPublishDirective   = "PUBLISH_DIRECTIVE"
	CmdCompleteDirective  = "COMPLETE_DIRECTIVE"
	CmdCleanupDirectives  = "CLEANUP_DIRECTIVES"
)

// DirectiveMaxAge is the maximum age for completed/failed directives before cleanup.
const DirectiveMaxAge = 24 * time.Hour

// Directive types.
const (
	DirectiveFence      = "FENCE"
	DirectivePromote    = "PROMOTE"
	DirectiveDemote     = "DEMOTE"
	DirectiveCheckpoint = "CHECKPOINT"
	DirectiveRewind     = "REWIND"
	DirectiveBasebackup = "BASEBACKUP"
)

// Directive status values.
const (
	DirectiveStatusPending   = "pending"
	DirectiveStatusCompleted = "completed"
	DirectiveStatusFailed    = "failed"
)

// Directive represents a command published by the Raft leader for a specific node to execute locally.
type Directive struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	TargetNode  string            `json:"target_node"`
	Params      map[string]string `json:"params,omitempty"`
	Status      string            `json:"status"`
	CreatedAt   time.Time         `json:"created_at"`
	CompletedAt time.Time         `json:"completed_at,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// FSMCommand is the wire format for commands applied to the FSM.
type FSMCommand struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// NodeInfo represents a node in the cluster as stored in the FSM.
type NodeInfo struct {
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	Port     int       `json:"port"`
	Role     string    `json:"role"` // "primary", "replica", "unknown"
	State    string    `json:"state"`
	Lag      int64     `json:"lag"`
	Timeline int64     `json:"timeline"`
	LastSeen time.Time `json:"last_seen"`
}

// ClusterConfig holds cluster-wide configuration in the FSM.
type ClusterConfig struct {
	SplitBrain bool `json:"split_brain"`
}

// FSMState is the in-memory state managed by the FSM.
type FSMState struct {
	Nodes         map[string]*NodeInfo  `json:"nodes"`
	PrimaryName   string                `json:"primary_name"`
	ClusterConfig ClusterConfig         `json:"cluster_config"`
	Directives    map[string]*Directive `json:"directives,omitempty"`
}

// ClusterFSM implements raft.FSM for pgbastion's cluster state.
type ClusterFSM struct {
	mu    sync.RWMutex
	state FSMState
}

// NewClusterFSM creates a new FSM with empty state.
func NewClusterFSM() *ClusterFSM {
	return &ClusterFSM{
		state: FSMState{
			Nodes:      make(map[string]*NodeInfo),
			Directives: make(map[string]*Directive),
		},
	}
}

// Apply applies a Raft log entry to the FSM.
func (f *ClusterFSM) Apply(log *raft.Log) interface{} {
	var cmd FSMCommand
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		return fmt.Errorf("unmarshal command: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch cmd.Type {
	case CmdSetPrimary:
		var node NodeInfo
		if err := json.Unmarshal(cmd.Data, &node); err != nil {
			return fmt.Errorf("unmarshal SET_PRIMARY data: %w", err)
		}
		node.Role = "primary"
		f.state.Nodes[node.Name] = &node
		f.state.PrimaryName = node.Name
		return nil

	case CmdSetReplica:
		var node NodeInfo
		if err := json.Unmarshal(cmd.Data, &node); err != nil {
			return fmt.Errorf("unmarshal SET_REPLICA data: %w", err)
		}
		node.Role = "replica"
		f.state.Nodes[node.Name] = &node
		return nil

	case CmdAddNode:
		var node NodeInfo
		if err := json.Unmarshal(cmd.Data, &node); err != nil {
			return fmt.Errorf("unmarshal ADD_NODE data: %w", err)
		}
		f.state.Nodes[node.Name] = &node
		return nil

	case CmdRemoveNode:
		var payload struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(cmd.Data, &payload); err != nil {
			return fmt.Errorf("unmarshal REMOVE_NODE data: %w", err)
		}
		delete(f.state.Nodes, payload.Name)
		if f.state.PrimaryName == payload.Name {
			f.state.PrimaryName = ""
		}
		return nil

	case CmdUpdateNodeHealth:
		var node NodeInfo
		if err := json.Unmarshal(cmd.Data, &node); err != nil {
			return fmt.Errorf("unmarshal UPDATE_NODE_HEALTH data: %w", err)
		}
		if existing, ok := f.state.Nodes[node.Name]; ok {
			existing.State = node.State
			existing.Lag = node.Lag
			existing.LastSeen = node.LastSeen
		}
		return nil

	case CmdSetClusterConfig:
		var cc ClusterConfig
		if err := json.Unmarshal(cmd.Data, &cc); err != nil {
			return fmt.Errorf("unmarshal SET_CLUSTER_CONFIG data: %w", err)
		}
		f.state.ClusterConfig = cc
		return nil

	case CmdPublishDirective:
		var d Directive
		if err := json.Unmarshal(cmd.Data, &d); err != nil {
			return fmt.Errorf("unmarshal PUBLISH_DIRECTIVE data: %w", err)
		}
		if f.state.Directives == nil {
			f.state.Directives = make(map[string]*Directive)
		}
		f.state.Directives[d.ID] = &d
		return nil

	case CmdCompleteDirective:
		var payload struct {
			ID    string `json:"id"`
			Error string `json:"error,omitempty"`
		}
		if err := json.Unmarshal(cmd.Data, &payload); err != nil {
			return fmt.Errorf("unmarshal COMPLETE_DIRECTIVE data: %w", err)
		}
		if d, ok := f.state.Directives[payload.ID]; ok {
			d.CompletedAt = time.Now()
			if payload.Error != "" {
				d.Status = DirectiveStatusFailed
				d.Error = payload.Error
			} else {
				d.Status = DirectiveStatusCompleted
			}
		}
		return nil

	case CmdCleanupDirectives:
		var payload struct {
			MaxAge time.Duration `json:"max_age"`
		}
		if err := json.Unmarshal(cmd.Data, &payload); err != nil {
			return fmt.Errorf("unmarshal CLEANUP_DIRECTIVES data: %w", err)
		}
		if payload.MaxAge == 0 {
			payload.MaxAge = DirectiveMaxAge
		}
		cutoff := time.Now().Add(-payload.MaxAge)
		cleaned := 0
		for id, d := range f.state.Directives {
			if d.Status != DirectiveStatusPending && !d.CompletedAt.IsZero() && d.CompletedAt.Before(cutoff) {
				delete(f.state.Directives, id)
				cleaned++
			}
		}
		return cleaned

	default:
		return fmt.Errorf("unknown command type: %s", cmd.Type)
	}
}

// Snapshot returns a snapshot of the FSM state.
func (f *ClusterFSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	data, err := json.Marshal(f.state)
	if err != nil {
		return nil, fmt.Errorf("marshal state: %w", err)
	}
	return &fsmSnapshot{data: data}, nil
}

// Restore replaces the FSM state from a snapshot.
func (f *ClusterFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()

	var state FSMState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	if state.Nodes == nil {
		state.Nodes = make(map[string]*NodeInfo)
	}
	if state.Directives == nil {
		state.Directives = make(map[string]*Directive)
	}

	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
	return nil
}

// GetPrimary returns the current primary node info, or nil.
func (f *ClusterFSM) GetPrimary() *NodeInfo {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.state.PrimaryName == "" {
		return nil
	}
	node, ok := f.state.Nodes[f.state.PrimaryName]
	if !ok {
		return nil
	}
	cp := *node
	return &cp
}

// GetReplicas returns all replica nodes.
func (f *ClusterFSM) GetReplicas() []*NodeInfo {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var replicas []*NodeInfo
	for _, node := range f.state.Nodes {
		if node.Role == "replica" {
			cp := *node
			replicas = append(replicas, &cp)
		}
	}
	return replicas
}

// IsSplitBrain returns the split-brain flag from cluster config.
func (f *ClusterFSM) IsSplitBrain() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.state.ClusterConfig.SplitBrain
}

// GetAllNodes returns a copy of all nodes.
func (f *ClusterFSM) GetAllNodes() map[string]*NodeInfo {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make(map[string]*NodeInfo, len(f.state.Nodes))
	for k, v := range f.state.Nodes {
		cp := *v
		result[k] = &cp
	}
	return result
}

// GetDirectivesForNode returns all pending directives targeting the given node.
func (f *ClusterFSM) GetDirectivesForNode(nodeName string) []*Directive {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var result []*Directive
	for _, d := range f.state.Directives {
		if d.TargetNode == nodeName && d.Status == DirectiveStatusPending {
			cp := *d
			result = append(result, &cp)
		}
	}
	return result
}

// GetDirective returns a directive by ID, or nil.
func (f *ClusterFSM) GetDirective(id string) *Directive {
	f.mu.RLock()
	defer f.mu.RUnlock()
	d, ok := f.state.Directives[id]
	if !ok {
		return nil
	}
	cp := *d
	return &cp
}

// GetAllDirectives returns a copy of all directives.
func (f *ClusterFSM) GetAllDirectives() map[string]*Directive {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make(map[string]*Directive, len(f.state.Directives))
	for k, v := range f.state.Directives {
		cp := *v
		result[k] = &cp
	}
	return result
}

// fsmSnapshot implements raft.FSMSnapshot.
type fsmSnapshot struct {
	data []byte
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// GetStaleDirectiveIDs returns IDs of completed/failed directives older than maxAge.
func (f *ClusterFSM) GetStaleDirectiveIDs(maxAge time.Duration) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	cutoff := time.Now().Add(-maxAge)
	var stale []string
	for id, d := range f.state.Directives {
		if d.Status != DirectiveStatusPending && !d.CompletedAt.IsZero() && d.CompletedAt.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	return stale
}

// DirectiveCount returns the total number of directives in the FSM.
func (f *ClusterFSM) DirectiveCount() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.state.Directives)
}
