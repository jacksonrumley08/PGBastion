package proxy

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// NodeRole represents the role of a PostgreSQL node.
type NodeRole string

const (
	RolePrimary NodeRole = "primary"
	RoleReplica NodeRole = "replica"
	RoleUnknown NodeRole = "unknown"
)

// NodeState represents the state of a single PostgreSQL node.
type NodeState struct {
	Name     string
	Host     string
	Port     int
	Role     NodeRole
	State    string
	Timeline int64
	Lag      int64
	LastSeen time.Time
}

// ClusterStateSource is the interface for reading cluster state.
type ClusterStateSource interface {
	GetPrimary() *NodeState
	GetReplicas() []*NodeState
	IsSplitBrain() bool
}

// Router determines the backend address for incoming connections
// based on the current cluster state.
type Router struct {
	mu       sync.RWMutex
	primary  string // host:port
	replicas []string
	rrIndex  uint64
	state    ClusterStateSource
	logger   *slog.Logger
}

// NewRouter creates a new Router backed by the given cluster state source.
func NewRouter(state ClusterStateSource, logger *slog.Logger) *Router {
	return &Router{
		state:  state,
		logger: logger.With("component", "router"),
	}
}

// PrimaryAddr returns the current primary backend address.
// Returns empty string if no primary is available or split-brain is detected.
func (r *Router) PrimaryAddr() string {
	if r.state.IsSplitBrain() {
		r.logger.Error("split-brain detected, refusing to route primary traffic")
		return ""
	}
	p := r.state.GetPrimary()
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", p.Host, p.Port)
}

// ReplicaAddr returns a replica backend address for read traffic.
// Uses simple round-robin selection.
func (r *Router) ReplicaAddr() string {
	if r.state.IsSplitBrain() {
		r.logger.Error("split-brain detected, refusing to route replica traffic")
		return ""
	}

	replicas := r.state.GetReplicas()
	if len(replicas) == 0 {
		// Fall back to primary for reads if no replicas.
		return r.PrimaryAddr()
	}

	addrs := make([]string, len(replicas))
	for i, rep := range replicas {
		addrs[i] = fmt.Sprintf("%s:%d", rep.Host, rep.Port)
	}

	r.mu.Lock()
	idx := r.rrIndex
	r.rrIndex++
	r.mu.Unlock()

	return addrs[idx%uint64(len(addrs))]
}

// RoutingTable returns the current routing table for observability.
func (r *Router) RoutingTable() RoutingTableInfo {
	primary := r.state.GetPrimary()
	replicas := r.state.GetReplicas()

	info := RoutingTableInfo{
		SplitBrain: r.state.IsSplitBrain(),
	}
	if primary != nil {
		info.Primary = fmt.Sprintf("%s:%d", primary.Host, primary.Port)
	}
	for _, rep := range replicas {
		info.Replicas = append(info.Replicas, fmt.Sprintf("%s:%d", rep.Host, rep.Port))
	}
	return info
}

// RoutingTableInfo is a snapshot of the routing table for API responses.
type RoutingTableInfo struct {
	Primary    string   `json:"primary"`
	Replicas   []string `json:"replicas"`
	SplitBrain bool     `json:"split_brain"`
}
