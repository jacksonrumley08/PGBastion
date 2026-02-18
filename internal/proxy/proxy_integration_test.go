package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// startEchoBackend starts a TCP server that echoes data back to the client.
// Returns the listener address and a cleanup function.
func startEchoBackend(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting echo backend: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	return ln.Addr().String(), func() { ln.Close() }
}

func TestProxy_EndToEnd_PrimaryRoute(t *testing.T) {
	backendAddr, cleanup := startEchoBackend(t)
	defer cleanup()

	// Parse the backend address.
	host, portStr, _ := net.SplitHostPort(backendAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// Set up cluster state with this backend as primary.
	state := &mockClusterState{
		primary: &NodeState{
			Name: "test-primary",
			Host: host,
			Port: port,
		},
	}

	router := NewRouter(state, testLogger())

	// Pick random ports for the proxy.
	primaryLn, _ := net.Listen("tcp", "127.0.0.1:0")
	replicaLn, _ := net.Listen("tcp", "127.0.0.1:0")
	primaryPort := primaryLn.Addr().(*net.TCPAddr).Port
	replicaPort := replicaLn.Addr().(*net.TCPAddr).Port
	primaryLn.Close()
	replicaLn.Close()

	p := NewProxy(primaryPort, replicaPort, 5*time.Second, 5*time.Second, router, nil, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)
	time.Sleep(100 * time.Millisecond) // Wait for listeners to start.

	// Connect to the proxy primary port.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", primaryPort), 2*time.Second)
	if err != nil {
		t.Fatalf("connecting to proxy: %v", err)
	}
	defer conn.Close()

	// Send data and verify echo.
	msg := []byte("hello pgbastion")
	_, err = conn.Write(msg)
	if err != nil {
		t.Fatalf("writing to proxy: %v", err)
	}

	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(conn, buf)
	if err != nil {
		t.Fatalf("reading from proxy: %v", err)
	}

	if string(buf) != string(msg) {
		t.Errorf("expected %q, got %q", msg, buf)
	}

	// Verify connection tracking.
	if p.Tracker().Count() != 1 {
		t.Errorf("expected 1 tracked connection, got %d", p.Tracker().Count())
	}

	conn.Close()
	time.Sleep(100 * time.Millisecond)

	if p.Tracker().Count() != 0 {
		t.Errorf("expected 0 tracked connections after close, got %d", p.Tracker().Count())
	}
}

func TestProxy_EndToEnd_ReplicaRoute(t *testing.T) {
	backendAddr, cleanup := startEchoBackend(t)
	defer cleanup()

	host, portStr, _ := net.SplitHostPort(backendAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	state := &mockClusterState{
		primary: &NodeState{Name: "primary", Host: "127.0.0.1", Port: 1},
		replicas: []*NodeState{
			{Name: "replica1", Host: host, Port: port},
		},
	}

	router := NewRouter(state, testLogger())

	primaryLn, _ := net.Listen("tcp", "127.0.0.1:0")
	replicaLn, _ := net.Listen("tcp", "127.0.0.1:0")
	primaryPort := primaryLn.Addr().(*net.TCPAddr).Port
	replicaPort := replicaLn.Addr().(*net.TCPAddr).Port
	primaryLn.Close()
	replicaLn.Close()

	p := NewProxy(primaryPort, replicaPort, 5*time.Second, 5*time.Second, router, nil, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", replicaPort), 2*time.Second)
	if err != nil {
		t.Fatalf("connecting to replica proxy: %v", err)
	}
	defer conn.Close()

	msg := []byte("read query")
	conn.Write(msg)

	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.ReadFull(conn, buf)

	if string(buf) != string(msg) {
		t.Errorf("expected %q, got %q", msg, buf)
	}
}

func TestProxy_EndToEnd_NoBackend(t *testing.T) {
	state := &mockClusterState{} // Empty, no primary.
	router := NewRouter(state, testLogger())

	primaryLn, _ := net.Listen("tcp", "127.0.0.1:0")
	replicaLn, _ := net.Listen("tcp", "127.0.0.1:0")
	primaryPort := primaryLn.Addr().(*net.TCPAddr).Port
	replicaPort := replicaLn.Addr().(*net.TCPAddr).Port
	primaryLn.Close()
	replicaLn.Close()

	p := NewProxy(primaryPort, replicaPort, 5*time.Second, 5*time.Second, router, nil, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	// Connection should be accepted but immediately closed by proxy (no backend).
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", primaryPort), 2*time.Second)
	if err != nil {
		t.Fatalf("connecting to proxy: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected connection to be closed by proxy")
	}
}

func TestProxy_EndToEnd_Drain(t *testing.T) {
	backendAddr, cleanup := startEchoBackend(t)
	defer cleanup()

	host, portStr, _ := net.SplitHostPort(backendAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	state := &mockClusterState{
		primary: &NodeState{Name: "primary", Host: host, Port: port},
	}

	router := NewRouter(state, testLogger())

	primaryLn, _ := net.Listen("tcp", "127.0.0.1:0")
	replicaLn, _ := net.Listen("tcp", "127.0.0.1:0")
	primaryPort := primaryLn.Addr().(*net.TCPAddr).Port
	replicaPort := replicaLn.Addr().(*net.TCPAddr).Port
	primaryLn.Close()
	replicaLn.Close()

	p := NewProxy(primaryPort, replicaPort, 2*time.Second, 5*time.Second, router, nil, testLogger())

	ctx, cancel := context.WithCancel(context.Background())

	go p.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	// Establish a connection.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", primaryPort), 2*time.Second)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close()

	// Verify it's connected.
	conn.Write([]byte("hi"))
	buf := make([]byte, 2)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.ReadFull(conn, buf)

	if p.Tracker().Count() != 1 {
		t.Fatalf("expected 1 connection before drain, got %d", p.Tracker().Count())
	}

	// Cancel context to trigger drain.
	cancel()
	time.Sleep(3 * time.Second)

	if p.Tracker().Count() != 0 {
		t.Errorf("expected 0 connections after drain, got %d", p.Tracker().Count())
	}
}

func TestProxy_EndToEnd_WithPooling(t *testing.T) {
	backendAddr, cleanup := startEchoBackend(t)
	defer cleanup()

	host, portStr, _ := net.SplitHostPort(backendAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	state := &mockClusterState{
		primary: &NodeState{Name: "primary", Host: host, Port: port},
	}

	router := NewRouter(state, testLogger())

	// Create a pool manager with pooling enabled.
	poolCfg := PoolConfig{
		Enabled:           true,
		MaxPoolSize:       10,
		MinIdleConns:      1,
		IdleTimeout:       5 * time.Minute,
		MaxConnLifetime:   30 * time.Minute,
		HealthCheckPeriod: 30 * time.Second,
		AcquireTimeout:    5 * time.Second,
	}
	poolManager := NewPoolManager(poolCfg, 5*time.Second, testLogger())
	defer poolManager.Close()

	primaryLn, _ := net.Listen("tcp", "127.0.0.1:0")
	replicaLn, _ := net.Listen("tcp", "127.0.0.1:0")
	primaryPort := primaryLn.Addr().(*net.TCPAddr).Port
	replicaPort := replicaLn.Addr().(*net.TCPAddr).Port
	primaryLn.Close()
	replicaLn.Close()

	p := NewProxy(primaryPort, replicaPort, 5*time.Second, 5*time.Second, router, poolManager, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	// Connect through the proxy (uses pooled connection).
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", primaryPort), 2*time.Second)
	if err != nil {
		t.Fatalf("connecting to proxy: %v", err)
	}
	defer conn.Close()

	// Send data and verify echo.
	msg := []byte("pooled hello")
	_, err = conn.Write(msg)
	if err != nil {
		t.Fatalf("writing to proxy: %v", err)
	}

	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(conn, buf)
	if err != nil {
		t.Fatalf("reading from proxy: %v", err)
	}

	if string(buf) != string(msg) {
		t.Errorf("expected %q, got %q", msg, buf)
	}

	// Verify pool manager has a pool for this backend.
	if p.PoolManager() == nil {
		t.Error("expected pool manager to be set")
	}
	if p.PoolManager().PoolCount() != 1 {
		t.Errorf("expected 1 pool, got %d", p.PoolManager().PoolCount())
	}
}
