package consensus

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// RaftOptions holds configurable Raft parameters.
type RaftOptions struct {
	LogLevel         string
	SnapshotRetain   int
	TransportTimeout time.Duration
	HeartbeatTimeout time.Duration
	ElectionTimeout  time.Duration
}

// DefaultRaftOptions returns sensible defaults for Raft configuration.
func DefaultRaftOptions() RaftOptions {
	return RaftOptions{
		LogLevel:         "WARN",
		SnapshotRetain:   2,
		TransportTimeout: 10 * time.Second,
		HeartbeatTimeout: 1 * time.Second,
		ElectionTimeout:  1 * time.Second,
	}
}

// NewRaftNode initializes and returns a Raft instance.
// If bootstrap is true and this is a fresh node (no existing log), it bootstraps a single-node cluster.
// The returned teardown function should be called on shutdown.
func NewRaftNode(nodeID, bindAddr string, bindPort int, dataDir string, bootstrap bool, opts RaftOptions, fsm raft.FSM, logger *slog.Logger) (*raft.Raft, func(), error) {
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(nodeID)
	raftCfg.LogLevel = opts.LogLevel
	if opts.HeartbeatTimeout > 0 {
		raftCfg.HeartbeatTimeout = opts.HeartbeatTimeout
	}
	if opts.ElectionTimeout > 0 {
		raftCfg.ElectionTimeout = opts.ElectionTimeout
	}

	// Ensure data directory exists.
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, nil, fmt.Errorf("create data dir: %w", err)
	}

	// Log store and stable store backed by BoltDB.
	boltPath := filepath.Join(dataDir, "raft.db")
	boltStore, err := raftboltdb.NewBoltStore(boltPath)
	if err != nil {
		return nil, nil, fmt.Errorf("new bolt store: %w", err)
	}

	// Snapshot store.
	snapshotRetain := opts.SnapshotRetain
	if snapshotRetain < 1 {
		snapshotRetain = 2
	}
	snapshotStore, err := raft.NewFileSnapshotStore(dataDir, snapshotRetain, os.Stderr)
	if err != nil {
		boltStore.Close()
		return nil, nil, fmt.Errorf("new snapshot store: %w", err)
	}

	// TCP transport.
	addr := fmt.Sprintf("%s:%d", bindAddr, bindPort)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		boltStore.Close()
		return nil, nil, fmt.Errorf("resolve tcp addr: %w", err)
	}

	transportTimeout := opts.TransportTimeout
	if transportTimeout == 0 {
		transportTimeout = 10 * time.Second
	}
	transport, err := raft.NewTCPTransport(tcpAddr.String(), tcpAddr, 3, transportTimeout, os.Stderr)
	if err != nil {
		boltStore.Close()
		return nil, nil, fmt.Errorf("new tcp transport: %w", err)
	}

	r, err := raft.NewRaft(raftCfg, fsm, boltStore, boltStore, snapshotStore, transport)
	if err != nil {
		transport.Close()
		boltStore.Close()
		return nil, nil, fmt.Errorf("new raft: %w", err)
	}

	// Bootstrap only if requested AND the log is empty (first-time startup).
	if bootstrap {
		lastIndex, _ := boltStore.LastIndex()
		if lastIndex == 0 {
			logger.Info("bootstrapping single-node raft cluster", "id", nodeID, "addr", addr)
			cfg := raft.Configuration{
				Servers: []raft.Server{
					{
						ID:      raft.ServerID(nodeID),
						Address: raft.ServerAddress(addr),
					},
				},
			}
			f := r.BootstrapCluster(cfg)
			if err := f.Error(); err != nil {
				r.Shutdown()
				transport.Close()
				boltStore.Close()
				return nil, nil, fmt.Errorf("bootstrap cluster: %w", err)
			}
		} else {
			logger.Info("raft log not empty, skipping bootstrap", "last_index", lastIndex)
		}
	}

	teardown := func() {
		r.Shutdown().Error()
		transport.Close()
		boltStore.Close()
	}

	logger.Info("raft node initialized", "id", nodeID, "addr", addr, "bootstrap", bootstrap)
	return r, teardown, nil
}
