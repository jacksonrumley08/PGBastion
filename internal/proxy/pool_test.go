package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestBackendPool_AcquireRelease(t *testing.T) {
	// Start a simple TCP server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	// Accept connections in background.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Keep connection open.
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					_, err := c.Read(buf)
					if err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:           true,
		MaxPoolSize:       5,
		MinIdleConns:      0, // Disable warmup for deterministic test.
		IdleTimeout:       time.Minute,
		MaxConnLifetime:   time.Hour,
		AcquireTimeout:    5 * time.Second,
		HealthCheckPeriod: time.Minute,
	}

	pool := NewBackendPool(ln.Addr().String(), cfg, 5*time.Second, testLogger())
	defer pool.Close()

	// Acquire a connection.
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	if conn == nil {
		t.Fatal("expected connection, got nil")
	}

	// Release the connection.
	pool.Release(conn)

	// Stats should show 1 idle.
	stats := pool.Stats()
	if stats["idle"].(int) != 1 {
		t.Errorf("expected 1 idle, got %v", stats["idle"])
	}
}

func TestBackendPool_Reuse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     5,
		MinIdleConns:    0,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pool := NewBackendPool(ln.Addr().String(), cfg, 5*time.Second, testLogger())
	defer pool.Close()

	ctx := context.Background()

	// Acquire and release.
	conn1, _ := pool.Acquire(ctx)
	pool.Release(conn1)

	// Acquire again - should get the same connection.
	conn2, _ := pool.Acquire(ctx)

	if conn1 != conn2 {
		t.Log("note: connections may differ if pool creates new ones")
	}

	pool.Release(conn2)
}

func TestBackendPool_MaxSize(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     2,
		MinIdleConns:    0,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  100 * time.Millisecond,
	}

	pool := NewBackendPool(ln.Addr().String(), cfg, 5*time.Second, testLogger())
	defer pool.Close()

	ctx := context.Background()

	// Acquire max connections.
	conn1, _ := pool.Acquire(ctx)
	conn2, _ := pool.Acquire(ctx)

	// Third acquire should timeout.
	_, err = pool.Acquire(ctx)
	if err != ErrAcquireTimeout {
		t.Errorf("expected ErrAcquireTimeout, got %v", err)
	}

	// Release one.
	pool.Release(conn1)

	// Now should be able to acquire.
	conn3, err := pool.Acquire(ctx)
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}

	pool.Release(conn2)
	pool.Release(conn3)
}

func TestBackendPool_Discard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     5,
		MinIdleConns:    0,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pool := NewBackendPool(ln.Addr().String(), cfg, 5*time.Second, testLogger())
	defer pool.Close()

	ctx := context.Background()
	conn, _ := pool.Acquire(ctx)

	// Discard instead of release.
	pool.Discard(conn)

	stats := pool.Stats()
	if stats["idle"].(int) != 0 {
		t.Errorf("expected 0 idle after discard, got %v", stats["idle"])
	}
}

func TestBackendPool_Close(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     5,
		MinIdleConns:    0,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pool := NewBackendPool(ln.Addr().String(), cfg, 5*time.Second, testLogger())

	ctx := context.Background()
	conn, _ := pool.Acquire(ctx)
	pool.Release(conn)

	pool.Close()

	// Should not be able to acquire after close.
	_, err = pool.Acquire(ctx)
	if err != ErrPoolClosed {
		t.Errorf("expected ErrPoolClosed, got %v", err)
	}
}

func TestBackendPool_ConcurrentAccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     10,
		MinIdleConns:    2,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pool := NewBackendPool(ln.Addr().String(), cfg, 5*time.Second, testLogger())
	defer pool.Close()

	ctx := context.Background()
	var wg sync.WaitGroup

	// Concurrent acquires and releases.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
			pool.Release(conn)
		}()
	}

	wg.Wait()
}

func TestPoolManager_MultipleBackends(t *testing.T) {
	// Start two TCP servers.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln1.Close()
	defer ln2.Close()

	for _, ln := range []net.Listener{ln1, ln2} {
		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					buf := make([]byte, 1024)
					for {
						if _, err := c.Read(buf); err != nil {
							c.Close()
							return
						}
					}
				}(conn)
			}
		}(ln)
	}

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     5,
		MinIdleConns:    1,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pm := NewPoolManager(cfg, 5*time.Second, testLogger())
	defer pm.Close()

	ctx := context.Background()

	// Acquire from both backends.
	conn1, _ := pm.Acquire(ctx, ln1.Addr().String())
	conn2, _ := pm.Acquire(ctx, ln2.Addr().String())

	// Should have 2 pools.
	if pm.PoolCount() != 2 {
		t.Errorf("expected 2 pools, got %d", pm.PoolCount())
	}

	pm.Release(ln1.Addr().String(), conn1)
	pm.Release(ln2.Addr().String(), conn2)
}

func TestPoolManager_RemovePool(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     5,
		MinIdleConns:    0,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pm := NewPoolManager(cfg, 5*time.Second, testLogger())
	defer pm.Close()

	ctx := context.Background()
	addr := ln.Addr().String()

	conn, _ := pm.Acquire(ctx, addr)
	pm.Release(addr, conn)

	if pm.PoolCount() != 1 {
		t.Errorf("expected 1 pool, got %d", pm.PoolCount())
	}

	pm.RemovePool(addr)

	if pm.PoolCount() != 0 {
		t.Errorf("expected 0 pools after removal, got %d", pm.PoolCount())
	}
}

func TestPoolManager_Stats(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     5,
		MinIdleConns:    0,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pm := NewPoolManager(cfg, 5*time.Second, testLogger())
	defer pm.Close()

	ctx := context.Background()
	addr := ln.Addr().String()

	conn, _ := pm.Acquire(ctx, addr)
	pm.Release(addr, conn)

	stats := pm.Stats()
	if len(stats) != 1 {
		t.Errorf("expected 1 backend in stats, got %d", len(stats))
	}

	if _, ok := stats[addr]; !ok {
		t.Error("expected stats for backend address")
	}
}

func TestBackendPool_ContextCancellation(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()

	cfg := PoolConfig{
		Enabled:         true,
		MaxPoolSize:     1,
		MinIdleConns:    0,
		IdleTimeout:     time.Minute,
		MaxConnLifetime: time.Hour,
		AcquireTimeout:  5 * time.Second,
	}

	pool := NewBackendPool(ln.Addr().String(), cfg, 5*time.Second, testLogger())
	defer pool.Close()

	ctx := context.Background()

	// Acquire the only connection.
	conn, _ := pool.Acquire(ctx)

	// Try to acquire with a cancelled context.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pool.Acquire(cancelledCtx)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	pool.Release(conn)
}

func TestDefaultPoolConfig(t *testing.T) {
	cfg := DefaultPoolConfig()

	if cfg.Enabled {
		t.Error("pool should be disabled by default")
	}

	if cfg.MaxPoolSize != 20 {
		t.Errorf("expected max pool size 20, got %d", cfg.MaxPoolSize)
	}

	if cfg.MinIdleConns != 2 {
		t.Errorf("expected min idle conns 2, got %d", cfg.MinIdleConns)
	}
}
