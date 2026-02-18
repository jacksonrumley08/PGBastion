package consensus

import (
	"crypto/tls"
	"crypto/x509"
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

	// TLS configuration for encrypted Raft transport.
	TLSEnabled bool
	TLSCert    string // Path to TLS certificate file
	TLSKey     string // Path to TLS private key file
	TLSCA      string // Path to CA certificate file for mutual TLS
}

// createTLSTransport creates a TLS-enabled Raft transport.
func createTLSTransport(tcpAddr *net.TCPAddr, timeout time.Duration, opts RaftOptions, logger *slog.Logger) (*raft.NetworkTransport, error) {
	// Load certificate and key.
	cert, err := tls.LoadX509KeyPair(opts.TLSCert, opts.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("loading TLS cert/key: %w", err)
	}

	// Create TLS config.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// If CA is provided, enable mutual TLS.
	if opts.TLSCA != "" {
		caCert, err := os.ReadFile(opts.TLSCA)
		if err != nil {
			return nil, fmt.Errorf("reading CA cert: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caPool
		tlsConfig.ClientCAs = caPool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		logger.Info("Raft mTLS enabled", "ca", opts.TLSCA)
	} else {
		logger.Info("Raft TLS enabled (no client verification)")
	}

	// Create TLS listener.
	listener, err := tls.Listen("tcp", tcpAddr.String(), tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("creating TLS listener: %w", err)
	}

	// Create a stream layer that uses TLS for dialing.
	stream := &tlsStreamLayer{
		listener:  listener,
		tlsConfig: tlsConfig,
		addr:      tcpAddr,
	}

	// Create network transport with the TLS stream layer.
	config := &raft.NetworkTransportConfig{
		Stream:                stream,
		MaxPool:               3,
		Timeout:               timeout,
		Logger:                nil,
		ServerAddressProvider: nil,
	}

	return raft.NewNetworkTransportWithConfig(config), nil
}

// tlsStreamLayer implements raft.StreamLayer with TLS.
type tlsStreamLayer struct {
	listener  net.Listener
	tlsConfig *tls.Config
	addr      *net.TCPAddr
}

func (t *tlsStreamLayer) Accept() (net.Conn, error) {
	return t.listener.Accept()
}

func (t *tlsStreamLayer) Close() error {
	return t.listener.Close()
}

func (t *tlsStreamLayer) Addr() net.Addr {
	return t.addr
}

func (t *tlsStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", string(address), t.tlsConfig)
	if err != nil {
		return nil, err
	}
	return conn, nil
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

	// TCP transport (optionally with TLS).
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

	var transport *raft.NetworkTransport
	if opts.TLSEnabled {
		transport, err = createTLSTransport(tcpAddr, transportTimeout, opts, logger)
	} else {
		transport, err = raft.NewTCPTransport(tcpAddr.String(), tcpAddr, 3, transportTimeout, os.Stderr)
	}
	if err != nil {
		boltStore.Close()
		return nil, nil, fmt.Errorf("new transport: %w", err)
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
