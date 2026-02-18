// Package testutil provides chaos testing utilities for error injection and failure simulation.
package testutil

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Common errors for chaos testing.
var (
	ErrChaosInjected     = errors.New("chaos: injected error")
	ErrChaosTimeout      = errors.New("chaos: operation timed out")
	ErrChaosConnRefused  = errors.New("chaos: connection refused")
	ErrChaosLeaderLost   = errors.New("chaos: leadership lost")
	ErrChaosNetworkSplit = errors.New("chaos: network partition")
)

// TestLogger returns a logger suitable for tests (error level only).
func TestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ChaosExecutor wraps a CommandExecutor with configurable failure injection.
type ChaosExecutor struct {
	mu sync.RWMutex

	// FailureRate is the probability (0.0-1.0) that a command will fail.
	FailureRate float64

	// Latency adds artificial delay to command execution.
	Latency time.Duration

	// FailCommands specifies which commands should always fail.
	// Key is the command name (e.g., "pg_ctl"), value is the error to return.
	FailCommands map[string]error

	// CommandCounts tracks how many times each command was called.
	CommandCounts map[string]int

	// FailAfterN makes commands fail after N successful calls.
	// Key is command name, value is the number of successes before failure.
	FailAfterN map[string]int

	// TimeoutCommands specifies commands that should simulate timeout.
	TimeoutCommands map[string]bool

	// outputs maps commands to their mock output.
	outputs map[string][]byte
}

// NewChaosExecutor creates a new ChaosExecutor with default settings.
func NewChaosExecutor() *ChaosExecutor {
	return &ChaosExecutor{
		FailCommands:    make(map[string]error),
		CommandCounts:   make(map[string]int),
		FailAfterN:      make(map[string]int),
		TimeoutCommands: make(map[string]bool),
		outputs:         make(map[string][]byte),
	}
}

// Run executes a command with chaos injection.
func (e *ChaosExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	// Extract base name for matching (allows matching "pg_ctl" for "/usr/lib/postgresql/bin/pg_ctl").
	baseName := filepath.Base(name)

	e.mu.Lock()
	// Track by base name (avoids double-counting when name == baseName).
	e.CommandCounts[baseName]++
	count := e.CommandCounts[baseName]

	// Check for timeout simulation (match either full path or base name).
	if e.TimeoutCommands[name] || e.TimeoutCommands[baseName] {
		e.mu.Unlock()
		<-ctx.Done()
		return nil, ErrChaosTimeout
	}

	// Check for specific command failures (match either full path or base name).
	if err, ok := e.FailCommands[name]; ok {
		e.mu.Unlock()
		return nil, err
	}
	if err, ok := e.FailCommands[baseName]; ok {
		e.mu.Unlock()
		return nil, err
	}

	// Check for fail-after-N (match base name or full path).
	if n, ok := e.FailAfterN[baseName]; ok && count > n {
		e.mu.Unlock()
		return nil, ErrChaosInjected
	}
	if n, ok := e.FailAfterN[name]; ok && count > n {
		e.mu.Unlock()
		return nil, ErrChaosInjected
	}

	// Apply random failure rate.
	if e.FailureRate > 0 && rand.Float64() < e.FailureRate {
		e.mu.Unlock()
		return nil, ErrChaosInjected
	}

	// Apply latency.
	latency := e.Latency
	output := e.outputs[name]
	if output == nil {
		output = e.outputs[baseName]
	}
	e.mu.Unlock()

	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return output, nil
}

// SetOutput sets the mock output for a command.
func (e *ChaosExecutor) SetOutput(name string, output []byte) {
	e.mu.Lock()
	e.outputs[name] = output
	e.mu.Unlock()
}

// Reset clears all chaos settings and counters.
func (e *ChaosExecutor) Reset() {
	e.mu.Lock()
	e.FailureRate = 0
	e.Latency = 0
	e.FailCommands = make(map[string]error)
	e.CommandCounts = make(map[string]int)
	e.FailAfterN = make(map[string]int)
	e.TimeoutCommands = make(map[string]bool)
	e.outputs = make(map[string][]byte)
	e.mu.Unlock()
}

// GetCallCount returns how many times a command was called.
func (e *ChaosExecutor) GetCallCount(name string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.CommandCounts[name]
}

// ChaosConnector implements PGConnector with failure injection.
type ChaosConnector struct {
	mu sync.RWMutex

	// FailConnect causes Connect() to fail.
	FailConnect bool

	// ConnectError is the error returned when FailConnect is true.
	ConnectError error

	// ConnectDelay adds latency to Connect().
	ConnectDelay time.Duration

	// Connection is the mock connection to return.
	Connection *ChaosConn

	// ConnectCount tracks connection attempts.
	ConnectCount int
}

// NewChaosConnector creates a new ChaosConnector.
func NewChaosConnector() *ChaosConnector {
	return &ChaosConnector{
		Connection: NewChaosConn(),
	}
}

// Connect creates a mock connection with chaos injection.
func (c *ChaosConnector) Connect(ctx context.Context, dsn string) (PGConn, error) {
	c.mu.Lock()
	c.ConnectCount++

	if c.ConnectDelay > 0 {
		delay := c.ConnectDelay
		c.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
	}

	if c.FailConnect {
		err := c.ConnectError
		if err == nil {
			err = ErrChaosConnRefused
		}
		c.mu.Unlock()
		return nil, err
	}

	conn := c.Connection
	c.mu.Unlock()
	return conn, nil
}

// PGConn interface for mock connections (matches cluster.PGConn).
type PGConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Close(ctx context.Context) error
}

// ChaosConn is a mock PostgreSQL connection with failure injection.
type ChaosConn struct {
	mu sync.RWMutex

	// FailQuery causes QueryRow/Query to fail.
	FailQuery bool

	// QueryError is the error returned when FailQuery is true.
	QueryError error

	// QueryDelay adds latency to queries.
	QueryDelay time.Duration

	// Results maps SQL queries to mock results.
	Results map[string]*MockRow

	// QueryHistory tracks all queries executed.
	QueryHistory []string

	// closed tracks whether Close() was called.
	closed bool
}

// NewChaosConn creates a new ChaosConn.
func NewChaosConn() *ChaosConn {
	return &ChaosConn{
		Results: make(map[string]*MockRow),
	}
}

// QueryRow returns a mock row with chaos injection.
func (c *ChaosConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.mu.Lock()
	c.QueryHistory = append(c.QueryHistory, sql)

	if c.QueryDelay > 0 {
		delay := c.QueryDelay
		c.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return &MockRow{err: ctx.Err()}
		}
		c.mu.Lock()
	}

	if c.FailQuery {
		err := c.QueryError
		if err == nil {
			err = ErrChaosInjected
		}
		c.mu.Unlock()
		return &MockRow{err: err}
	}

	row := c.Results[sql]
	c.mu.Unlock()

	if row == nil {
		return &MockRow{err: pgx.ErrNoRows}
	}
	return row
}

// Query returns mock rows with chaos injection.
func (c *ChaosConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.mu.Lock()
	c.QueryHistory = append(c.QueryHistory, sql)

	if c.FailQuery {
		err := c.QueryError
		if err == nil {
			err = ErrChaosInjected
		}
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	return &MockRows{}, nil
}

// Close closes the mock connection.
func (c *ChaosConn) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// IsClosed returns whether Close() was called.
func (c *ChaosConn) IsClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}

// SetResult sets a mock result for a query.
func (c *ChaosConn) SetResult(sql string, values ...any) {
	c.mu.Lock()
	c.Results[sql] = &MockRow{values: values}
	c.mu.Unlock()
}

// MockRow implements pgx.Row for testing.
type MockRow struct {
	values []any
	err    error
}

// Scan copies mock values into destinations.
func (r *MockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, v := range r.values {
		if i < len(dest) {
			switch d := dest[i].(type) {
			case *bool:
				if val, ok := v.(bool); ok {
					*d = val
				}
			case *int:
				if val, ok := v.(int); ok {
					*d = val
				}
			case *int64:
				if val, ok := v.(int64); ok {
					*d = val
				}
			case **int64:
				if val, ok := v.(*int64); ok {
					*d = val
				} else if val, ok := v.(int64); ok {
					*d = &val
				}
			case *string:
				if val, ok := v.(string); ok {
					*d = val
				}
			}
		}
	}
	return nil
}

// MockRows implements a minimal pgx.Rows for testing.
type MockRows struct {
	rows    [][]any
	current int
	closed  bool
}

func (r *MockRows) Close()                                        { r.closed = true }
func (r *MockRows) Err() error                                    { return nil }
func (r *MockRows) CommandTag() pgconn.CommandTag                 { return pgconn.CommandTag{} }
func (r *MockRows) FieldDescriptions() []pgconn.FieldDescription  { return nil }
func (r *MockRows) RawValues() [][]byte                           { return nil }
func (r *MockRows) Conn() *pgx.Conn                               { return nil }
func (r *MockRows) Values() ([]any, error)                        { return nil, nil }

func (r *MockRows) Next() bool {
	if r.current >= len(r.rows) {
		return false
	}
	r.current++
	return true
}

func (r *MockRows) Scan(dest ...any) error {
	if r.current == 0 || r.current > len(r.rows) {
		return pgx.ErrNoRows
	}
	row := r.rows[r.current-1]
	for i, v := range row {
		if i < len(dest) {
			switch d := dest[i].(type) {
			case *string:
				if val, ok := v.(string); ok {
					*d = val
				}
			case *int64:
				if val, ok := v.(int64); ok {
					*d = val
				}
			case *bool:
				if val, ok := v.(bool); ok {
					*d = val
				}
			}
		}
	}
	return nil
}

// ChaosLeadershipSource implements LeadershipSource with failure injection.
type ChaosLeadershipSource struct {
	mu sync.RWMutex

	// IsLeader indicates whether this node is currently the leader.
	IsLeader bool

	// FailCampaign causes Campaign() to fail.
	FailCampaign bool

	// CampaignError is the error returned when FailCampaign is true.
	CampaignError error

	// CampaignDelay adds latency to Campaign().
	CampaignDelay time.Duration

	// FailResign causes Resign() to fail.
	FailResign bool

	// ResignError is the error returned when FailResign is true.
	ResignError error

	// leadershipLost is signaled when leadership is lost.
	leadershipLost chan struct{}

	// campaignCount tracks campaign attempts.
	campaignCount int32
}

// NewChaosLeadershipSource creates a new ChaosLeadershipSource.
func NewChaosLeadershipSource() *ChaosLeadershipSource {
	return &ChaosLeadershipSource{
		leadershipLost: make(chan struct{}),
	}
}

// Campaign attempts to become the leader with chaos injection.
func (s *ChaosLeadershipSource) Campaign(ctx context.Context) error {
	atomic.AddInt32(&s.campaignCount, 1)

	s.mu.RLock()
	delay := s.CampaignDelay
	failCampaign := s.FailCampaign
	campaignError := s.CampaignError
	s.mu.RUnlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if failCampaign {
		if campaignError != nil {
			return campaignError
		}
		return ErrChaosInjected
	}

	s.mu.Lock()
	s.IsLeader = true
	s.mu.Unlock()

	return nil
}

// Resign gives up leadership with chaos injection.
func (s *ChaosLeadershipSource) Resign(ctx context.Context) error {
	s.mu.RLock()
	failResign := s.FailResign
	resignError := s.ResignError
	s.mu.RUnlock()

	if failResign {
		if resignError != nil {
			return resignError
		}
		return ErrChaosInjected
	}

	s.mu.Lock()
	s.IsLeader = false
	s.mu.Unlock()

	return nil
}

// LeadershipLost returns a channel signaled when leadership is lost.
func (s *ChaosLeadershipSource) LeadershipLost() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leadershipLost
}

// SimulateLeadershipLoss signals that leadership was lost.
func (s *ChaosLeadershipSource) SimulateLeadershipLoss() {
	s.mu.Lock()
	s.IsLeader = false
	close(s.leadershipLost)
	s.leadershipLost = make(chan struct{})
	s.mu.Unlock()
}

// GetCampaignCount returns how many times Campaign() was called.
func (s *ChaosLeadershipSource) GetCampaignCount() int {
	return int(atomic.LoadInt32(&s.campaignCount))
}

// Reset resets the leadership source state.
func (s *ChaosLeadershipSource) Reset() {
	s.mu.Lock()
	s.IsLeader = false
	s.FailCampaign = false
	s.CampaignError = nil
	s.CampaignDelay = 0
	s.FailResign = false
	s.ResignError = nil
	s.leadershipLost = make(chan struct{})
	atomic.StoreInt32(&s.campaignCount, 0)
	s.mu.Unlock()
}

// ChaosClusterState implements ClusterStateSource with failure injection.
type ChaosClusterState struct {
	mu sync.RWMutex

	Primary    *NodeState
	Replicas   []*NodeState
	SplitBrain bool

	// SimulatePartition causes IsSplitBrain() to return true.
	SimulatePartition bool

	// FlickerPrimary causes GetPrimary() to alternate between primary and nil.
	FlickerPrimary bool
	flickerCount   int
}

// NodeState represents a cluster node (matches proxy.NodeState).
type NodeState struct {
	Name string
	Host string
	Port int
}

// NewChaosClusterState creates a new ChaosClusterState.
func NewChaosClusterState() *ChaosClusterState {
	return &ChaosClusterState{}
}

// GetPrimary returns the primary node with chaos injection.
func (s *ChaosClusterState) GetPrimary() *NodeState {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.FlickerPrimary {
		s.flickerCount++
		if s.flickerCount%2 == 0 {
			return nil
		}
	}

	return s.Primary
}

// GetReplicas returns replica nodes.
func (s *ChaosClusterState) GetReplicas() []*NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Replicas
}

// IsSplitBrain returns whether the cluster is in split-brain.
func (s *ChaosClusterState) IsSplitBrain() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.SplitBrain || s.SimulatePartition
}

// SetPrimary sets the primary node.
func (s *ChaosClusterState) SetPrimary(name, host string, port int) {
	s.mu.Lock()
	s.Primary = &NodeState{Name: name, Host: host, Port: port}
	s.mu.Unlock()
}

// AddReplica adds a replica node.
func (s *ChaosClusterState) AddReplica(name, host string, port int) {
	s.mu.Lock()
	s.Replicas = append(s.Replicas, &NodeState{Name: name, Host: host, Port: port})
	s.mu.Unlock()
}

// Reset clears all state.
func (s *ChaosClusterState) Reset() {
	s.mu.Lock()
	s.Primary = nil
	s.Replicas = nil
	s.SplitBrain = false
	s.SimulatePartition = false
	s.FlickerPrimary = false
	s.flickerCount = 0
	s.mu.Unlock()
}
