package proxy

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ConnStats tracks per-connection statistics.
type ConnStats struct {
	BytesSent     atomic.Int64
	BytesReceived atomic.Int64
	StartTime     time.Time
	ClientAddr    string
	BackendAddr   string
}

// ConnectionTracker manages active proxy connections.
type ConnectionTracker struct {
	mu     sync.Mutex
	conns  map[uint64]*trackedConn
	nextID atomic.Uint64
	logger *slog.Logger
}

type trackedConn struct {
	id         uint64
	clientConn net.Conn
	backendConn net.Conn
	stats      *ConnStats
	cancel     func()
}

// NewConnectionTracker creates a new connection tracker.
func NewConnectionTracker(logger *slog.Logger) *ConnectionTracker {
	return &ConnectionTracker{
		conns:  make(map[uint64]*trackedConn),
		logger: logger.With("component", "conn-tracker"),
	}
}

// Add registers a new proxied connection pair and returns its ID.
func (ct *ConnectionTracker) Add(clientConn, backendConn net.Conn, cancel func()) uint64 {
	id := ct.nextID.Add(1)
	ct.mu.Lock()
	ct.conns[id] = &trackedConn{
		id:          id,
		clientConn:  clientConn,
		backendConn: backendConn,
		stats: &ConnStats{
			StartTime:   time.Now(),
			ClientAddr:  clientConn.RemoteAddr().String(),
			BackendAddr: backendConn.RemoteAddr().String(),
		},
		cancel: cancel,
	}
	ct.mu.Unlock()
	return id
}

// Remove unregisters a connection.
func (ct *ConnectionTracker) Remove(id uint64) {
	ct.mu.Lock()
	delete(ct.conns, id)
	ct.mu.Unlock()
}

// Count returns the number of active connections.
func (ct *ConnectionTracker) Count() int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return len(ct.conns)
}

// ConnectionsByBackend returns a map of backend address to connection count.
func (ct *ConnectionTracker) ConnectionsByBackend() map[string]int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	result := make(map[string]int)
	for _, c := range ct.conns {
		result[c.stats.BackendAddr]++
	}
	return result
}

// DrainAll closes all active connections gracefully.
func (ct *ConnectionTracker) DrainAll(timeout time.Duration) {
	ct.mu.Lock()
	conns := make([]*trackedConn, 0, len(ct.conns))
	for _, c := range ct.conns {
		conns = append(conns, c)
	}
	ct.mu.Unlock()

	ct.logger.Info("draining connections", "count", len(conns), "timeout", timeout)

	for _, c := range conns {
		c.cancel()
	}

	// Wait for connections to close up to the timeout.
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			remaining := ct.Count()
			if remaining > 0 {
				ct.logger.Warn("drain timeout reached, force closing remaining connections", "remaining", remaining)
				ct.forceCloseAll()
			}
			return
		case <-ticker.C:
			if ct.Count() == 0 {
				ct.logger.Info("all connections drained")
				return
			}
		}
	}
}

func (ct *ConnectionTracker) forceCloseAll() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	for _, c := range ct.conns {
		c.clientConn.Close()
		c.backendConn.Close()
	}
}

// halfCloser is an interface for connections that support half-close.
type halfCloser interface {
	CloseWrite() error
}

// Pipe bidirectionally copies data between two connections.
// Returns when either direction encounters an error or EOF.
func Pipe(client, backend net.Conn, stats *ConnStats, logger *slog.Logger) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, _ := io.Copy(backend, client)
		stats.BytesSent.Add(n)
		if hc, ok := backend.(halfCloser); ok {
			hc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, backend)
		stats.BytesReceived.Add(n)
		if hc, ok := client.(halfCloser); ok {
			hc.CloseWrite()
		}
	}()

	wg.Wait()
}
