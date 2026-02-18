package consensus

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jrumley/pgbastion/internal/proxy"
)

// ErrNotLeader is returned when a write is attempted on a non-leader node.
var ErrNotLeader = errors.New("not the raft leader")

// StateWriter is the interface for writing cluster state to the consensus layer.
type StateWriter interface {
	SetPrimary(node NodeInfo) error
	SetReplica(node NodeInfo) error
	SetClusterConfig(cfg ClusterConfig) error
}

// Store provides a clean API over the Raft FSM for the rest of the application.
type Store struct {
	raft *raft.Raft
	fsm  *ClusterFSM
}

// NewStore creates a new Store wrapping a Raft instance and its FSM.
func NewStore(r *raft.Raft, fsm *ClusterFSM) *Store {
	return &Store{raft: r, fsm: fsm}
}

func (s *Store) apply(cmd FSMCommand, timeout time.Duration) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	start := time.Now()
	f := s.raft.Apply(data, timeout)
	ObserveApplyLatency(time.Since(start))
	if f.Error() != nil {
		return fmt.Errorf("raft apply: %w", f.Error())
	}
	if resp, ok := f.Response().(error); ok && resp != nil {
		return fmt.Errorf("fsm apply: %w", resp)
	}
	return nil
}

func marshalData(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// SetPrimary sets the primary node in the FSM.
func (s *Store) SetPrimary(node NodeInfo) error {
	data, err := marshalData(node)
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdSetPrimary, Data: data}, 5*time.Second)
}

// SetReplica sets a replica node in the FSM.
func (s *Store) SetReplica(node NodeInfo) error {
	data, err := marshalData(node)
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdSetReplica, Data: data}, 5*time.Second)
}

// SetClusterConfig sets the cluster-wide configuration.
func (s *Store) SetClusterConfig(cfg ClusterConfig) error {
	data, err := marshalData(cfg)
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdSetClusterConfig, Data: data}, 5*time.Second)
}

// UpdateNodeHealth updates a node's health information in the FSM.
func (s *Store) UpdateNodeHealth(node NodeInfo) error {
	data, err := marshalData(node)
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdUpdateNodeHealth, Data: data}, 5*time.Second)
}

// AddNode adds a node to the FSM state.
func (s *Store) AddNode(node NodeInfo) error {
	data, err := marshalData(node)
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdAddNode, Data: data}, 5*time.Second)
}

// RemoveNode removes a node from the FSM state.
func (s *Store) RemoveNode(name string) error {
	data, err := marshalData(struct {
		Name string `json:"name"`
	}{Name: name})
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdRemoveNode, Data: data}, 5*time.Second)
}

// PublishDirective publishes a directive to the Raft FSM. Only the leader can publish.
func (s *Store) PublishDirective(d Directive) error {
	data, err := marshalData(d)
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdPublishDirective, Data: data}, 5*time.Second)
}

// CompleteDirective marks a directive as completed or failed.
func (s *Store) CompleteDirective(id string, errMsg string) error {
	data, err := marshalData(struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}{ID: id, Error: errMsg})
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdCompleteDirective, Data: data}, 5*time.Second)
}

// SetPostgreSQLConfig sets the cluster-wide PostgreSQL configuration (full replace).
func (s *Store) SetPostgreSQLConfig(cfg PostgreSQLConfig) error {
	data, err := marshalData(cfg)
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdSetPostgreSQLConfig, Data: data}, 5*time.Second)
}

// PatchPostgreSQLConfig updates specific PostgreSQL parameters (merge).
// Empty values in the map will delete the parameter.
func (s *Store) PatchPostgreSQLConfig(params map[string]string, updatedBy string) error {
	data, err := marshalData(struct {
		Parameters map[string]string `json:"parameters"`
		UpdatedBy  string            `json:"updated_by,omitempty"`
	}{Parameters: params, UpdatedBy: updatedBy})
	if err != nil {
		return err
	}
	return s.apply(FSMCommand{Type: CmdPatchPostgreSQLConfig, Data: data}, 5*time.Second)
}

// GetPostgreSQLConfig returns the current PostgreSQL configuration from FSM.
func (s *Store) GetPostgreSQLConfig() PostgreSQLConfig {
	return s.fsm.GetPostgreSQLConfig()
}

// GetPostgreSQLConfigVersion returns the current PostgreSQL config version.
func (s *Store) GetPostgreSQLConfigVersion() int64 {
	return s.fsm.GetPostgreSQLConfigVersion()
}

// CleanupDirectives removes completed/failed directives older than maxAge.
// Returns the number of directives cleaned up.
func (s *Store) CleanupDirectives(maxAge time.Duration) (int, error) {
	data, err := marshalData(struct {
		MaxAge time.Duration `json:"max_age"`
	}{MaxAge: maxAge})
	if err != nil {
		return 0, err
	}
	if s.raft.State() != raft.Leader {
		return 0, ErrNotLeader
	}
	cmdData, err := json.Marshal(FSMCommand{Type: CmdCleanupDirectives, Data: data})
	if err != nil {
		return 0, fmt.Errorf("marshal command: %w", err)
	}
	f := s.raft.Apply(cmdData, 5*time.Second)
	if f.Error() != nil {
		return 0, fmt.Errorf("raft apply: %w", f.Error())
	}
	if count, ok := f.Response().(int); ok {
		return count, nil
	}
	if resp, ok := f.Response().(error); ok && resp != nil {
		return 0, fmt.Errorf("fsm apply: %w", resp)
	}
	return 0, nil
}

// GetStaleDirectiveCount returns the number of directives eligible for cleanup.
func (s *Store) GetStaleDirectiveCount(maxAge time.Duration) int {
	return len(s.fsm.GetStaleDirectiveIDs(maxAge))
}

// GetDirectivesForNode returns pending directives for the given node from the FSM.
func (s *Store) GetDirectivesForNode(nodeName string) []*Directive {
	return s.fsm.GetDirectivesForNode(nodeName)
}

// GetDirective returns a directive by ID from the FSM.
func (s *Store) GetDirective(id string) *Directive {
	return s.fsm.GetDirective(id)
}

// GetAllDirectives returns all directives from the FSM.
func (s *Store) GetAllDirectives() map[string]*Directive {
	return s.fsm.GetAllDirectives()
}

// IsLeader returns true if this node is the Raft leader.
func (s *Store) IsLeader() bool {
	return s.raft.State() == raft.Leader
}

// LeaderAddr returns the address of the current Raft leader.
func (s *Store) LeaderAddr() string {
	addr, _ := s.raft.LeaderWithID()
	return string(addr)
}

// Stats returns Raft statistics.
func (s *Store) Stats() map[string]string {
	return s.raft.Stats()
}

// Peers returns the current Raft configuration servers.
func (s *Store) Peers() ([]raft.Server, error) {
	f := s.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, err
	}
	return f.Configuration().Servers, nil
}

// AddPeer adds a voting peer to the Raft cluster.
func (s *Store) AddPeer(id, address string) error {
	f := s.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(address), 0, 10*time.Second)
	return f.Error()
}

// RemovePeer removes a peer from the Raft cluster.
func (s *Store) RemovePeer(id string) error {
	f := s.raft.RemoveServer(raft.ServerID(id), 0, 10*time.Second)
	return f.Error()
}

// TransferLeadership transfers leadership to another node.
func (s *Store) TransferLeadership() error {
	f := s.raft.LeadershipTransfer()
	return f.Error()
}

// RaftInstance returns the underlying Raft instance.
func (s *Store) RaftInstance() *raft.Raft {
	return s.raft
}

// FSM returns the underlying ClusterFSM for read access.
func (s *Store) FSM() *ClusterFSM {
	return s.fsm
}

// RaftState returns the current Raft state (leader, follower, candidate).
func (s *Store) RaftState() string {
	return s.raft.State().String()
}

// LastContact returns the time since last contact with the leader.
// Returns 0 if this node is the leader or if we've never contacted a leader.
func (s *Store) LastContact() time.Duration {
	stats := s.raft.Stats()
	lastContact := stats["last_contact"]
	if lastContact == "" || lastContact == "never" || lastContact == "0" {
		return 0
	}
	d, err := time.ParseDuration(lastContact)
	if err != nil {
		return 0
	}
	return d
}

// HasQuorum returns true if this node believes it has quorum.
// For leaders, this is always true (they wouldn't be leader otherwise).
// For followers, we check if we've heard from a leader recently.
func (s *Store) HasQuorum(timeout time.Duration) bool {
	state := s.raft.State()
	if state == raft.Leader {
		return true
	}

	// For followers/candidates, check when we last heard from a leader.
	lastContact := s.LastContact()
	if lastContact == 0 {
		// Never contacted leader - might be bootstrapping or isolated.
		// Check if there's a known leader.
		leaderAddr, _ := s.raft.LeaderWithID()
		return leaderAddr != ""
	}

	return lastContact < timeout
}

// RaftStateAdapter adapts the Store to satisfy the proxy's ClusterStateSource interface.
// It reads from the local FSM replica for routing decisions.
type RaftStateAdapter struct {
	fsm *ClusterFSM
}

// NewRaftStateAdapter creates an adapter that reads from the FSM.
func NewRaftStateAdapter(fsm *ClusterFSM) *RaftStateAdapter {
	return &RaftStateAdapter{fsm: fsm}
}

// GetPrimary returns the primary node as a proxy.NodeState.
func (a *RaftStateAdapter) GetPrimary() *proxy.NodeState {
	node := a.fsm.GetPrimary()
	if node == nil {
		return nil
	}
	return &proxy.NodeState{
		Name:     node.Name,
		Host:     node.Host,
		Port:     node.Port,
		Role:     proxy.RolePrimary,
		State:    node.State,
		Timeline: node.Timeline,
		Lag:      node.Lag,
		LastSeen: node.LastSeen,
	}
}

// GetReplicas returns replicas as proxy.NodeState slices.
func (a *RaftStateAdapter) GetReplicas() []*proxy.NodeState {
	nodes := a.fsm.GetReplicas()
	result := make([]*proxy.NodeState, len(nodes))
	for i, n := range nodes {
		result[i] = &proxy.NodeState{
			Name:     n.Name,
			Host:     n.Host,
			Port:     n.Port,
			Role:     proxy.RoleReplica,
			State:    n.State,
			Timeline: n.Timeline,
			Lag:      n.Lag,
			LastSeen: n.LastSeen,
		}
	}
	return result
}

// IsSplitBrain returns the split-brain flag from the FSM.
func (a *RaftStateAdapter) IsSplitBrain() bool {
	return a.fsm.IsSplitBrain()
}
