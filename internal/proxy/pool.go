package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Pool errors.
var (
	ErrPoolClosed         = errors.New("pool: pool is closed")
	ErrAcquireTimeout     = errors.New("pool: acquire timeout")
	ErrPoolExhausted      = errors.New("pool: no available connections")
	ErrConnectionUnhealthy = errors.New("pool: connection failed health check")
)

// Pool metrics.
var (
	poolConnectionsIdle = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgbastion_pool_connections_idle",
		Help: "Number of idle connections in the pool",
	}, []string{"backend"})

	poolConnectionsInUse = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgbastion_pool_connections_in_use",
		Help: "Number of connections currently in use",
	}, []string{"backend"})

	poolConnectionsAcquiredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_pool_connections_acquired_total",
		Help: "Total number of connection acquisitions",
	}, []string{"backend", "source"})

	poolWaitDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pgbastion_pool_wait_duration_seconds",
		Help:    "Time spent waiting to acquire a connection",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms to ~1s
	}, []string{"backend"})

	poolConnectionErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_pool_connection_errors_total",
		Help: "Total number of connection errors",
	}, []string{"backend", "type"})
)

// PoolConfig holds connection pool configuration.
type PoolConfig struct {
	Enabled           bool          `yaml:"enabled"`
	MaxPoolSize       int           `yaml:"max_pool_size"`       // per backend
	MinIdleConns      int           `yaml:"min_idle_conns"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxConnLifetime   time.Duration `yaml:"max_conn_lifetime"`
	HealthCheckPeriod time.Duration `yaml:"health_check_period"`
	AcquireTimeout    time.Duration `yaml:"acquire_timeout"`
}

// DefaultPoolConfig returns the default pool configuration.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		Enabled:           false,
		MaxPoolSize:       20,
		MinIdleConns:      2,
		IdleTimeout:       5 * time.Minute,
		MaxConnLifetime:   30 * time.Minute,
		HealthCheckPeriod: 30 * time.Second,
		AcquireTimeout:    5 * time.Second,
	}
}

// pooledConn wraps a connection with pool metadata.
type pooledConn struct {
	conn        net.Conn
	createdAt   time.Time
	lastUsedAt  time.Time
	usageCount  int64
	backendAddr string
}

// BackendPool manages a pool of connections to a single backend.
type BackendPool struct {
	address     string
	config      PoolConfig
	logger      *slog.Logger
	dialTimeout time.Duration

	mu          sync.Mutex
	idle        []*pooledConn
	inUse       map[net.Conn]*pooledConn
	waiters     []chan *pooledConn
	totalConns  int32
	closed      bool

	// Background goroutine control.
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewBackendPool creates a new connection pool for a backend.
func NewBackendPool(address string, cfg PoolConfig, dialTimeout time.Duration, logger *slog.Logger) *BackendPool {
	// Apply defaults for zero values to prevent panics.
	if cfg.HealthCheckPeriod <= 0 {
		cfg.HealthCheckPeriod = 30 * time.Second
	}
	if cfg.MaxPoolSize <= 0 {
		cfg.MaxPoolSize = 20
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.MaxConnLifetime <= 0 {
		cfg.MaxConnLifetime = 30 * time.Minute
	}
	if cfg.AcquireTimeout <= 0 {
		cfg.AcquireTimeout = 5 * time.Second
	}

	p := &BackendPool{
		address:     address,
		config:      cfg,
		logger:      logger.With("component", "pool", "backend", address),
		dialTimeout: dialTimeout,
		idle:        make([]*pooledConn, 0, cfg.MaxPoolSize),
		inUse:       make(map[net.Conn]*pooledConn),
		done:        make(chan struct{}),
	}

	// Start background maintenance.
	p.wg.Add(1)
	go p.maintenance()

	// Pre-warm minimum idle connections.
	go p.warmUp()

	return p
}

// Acquire gets a connection from the pool, creating one if necessary.
func (p *BackendPool) Acquire(ctx context.Context) (net.Conn, error) {
	start := time.Now()
	defer func() {
		poolWaitDuration.WithLabelValues(p.address).Observe(time.Since(start).Seconds())
	}()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}

	// Try to get an idle connection.
	for len(p.idle) > 0 {
		conn := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]

		// Check if connection is still healthy.
		if p.isConnectionHealthy(conn) {
			conn.lastUsedAt = time.Now()
			conn.usageCount++
			p.inUse[conn.conn] = conn
			p.mu.Unlock()

			poolConnectionsIdle.WithLabelValues(p.address).Set(float64(len(p.idle)))
			poolConnectionsInUse.WithLabelValues(p.address).Set(float64(len(p.inUse)))
			poolConnectionsAcquiredTotal.WithLabelValues(p.address, "pool").Inc()

			return conn.conn, nil
		}

		// Connection is unhealthy, close it.
		p.closeConn(conn)
	}

	// No idle connections. Can we create a new one?
	if int(atomic.LoadInt32(&p.totalConns)) < p.config.MaxPoolSize {
		atomic.AddInt32(&p.totalConns, 1)
		p.mu.Unlock()

		conn, err := p.dial(ctx)
		if err != nil {
			atomic.AddInt32(&p.totalConns, -1)
			poolConnectionErrors.WithLabelValues(p.address, "dial").Inc()
			return nil, err
		}

		pc := &pooledConn{
			conn:        conn,
			createdAt:   time.Now(),
			lastUsedAt:  time.Now(),
			usageCount:  1,
			backendAddr: p.address,
		}

		p.mu.Lock()
		p.inUse[conn] = pc
		p.mu.Unlock()

		poolConnectionsInUse.WithLabelValues(p.address).Set(float64(len(p.inUse)))
		poolConnectionsAcquiredTotal.WithLabelValues(p.address, "new").Inc()

		return conn, nil
	}

	// Pool exhausted, wait for a connection.
	waiter := make(chan *pooledConn, 1)
	p.waiters = append(p.waiters, waiter)
	p.mu.Unlock()

	// Set up timeout.
	timeout := p.config.AcquireTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case conn := <-waiter:
		if conn != nil {
			conn.lastUsedAt = time.Now()
			conn.usageCount++

			p.mu.Lock()
			p.inUse[conn.conn] = conn
			p.mu.Unlock()

			poolConnectionsInUse.WithLabelValues(p.address).Set(float64(len(p.inUse)))
			poolConnectionsAcquiredTotal.WithLabelValues(p.address, "waiter").Inc()

			return conn.conn, nil
		}
		return nil, ErrPoolExhausted

	case <-timer.C:
		p.mu.Lock()
		// Remove ourselves from waiters.
		for i, w := range p.waiters {
			if w == waiter {
				p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		poolConnectionErrors.WithLabelValues(p.address, "timeout").Inc()
		return nil, ErrAcquireTimeout

	case <-ctx.Done():
		p.mu.Lock()
		for i, w := range p.waiters {
			if w == waiter {
				p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Release returns a connection to the pool.
func (p *BackendPool) Release(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pc, ok := p.inUse[conn]
	if !ok {
		// Connection not from this pool, just close it.
		conn.Close()
		return
	}

	delete(p.inUse, conn)
	poolConnectionsInUse.WithLabelValues(p.address).Set(float64(len(p.inUse)))

	if p.closed {
		p.closeConn(pc)
		return
	}

	// Check if connection should be returned to pool.
	if !p.isConnectionHealthy(pc) {
		p.closeConn(pc)
		return
	}

	// First, try to give to a waiter.
	if len(p.waiters) > 0 {
		waiter := p.waiters[0]
		p.waiters = p.waiters[1:]
		waiter <- pc
		return
	}

	// Return to idle pool.
	pc.lastUsedAt = time.Now()
	p.idle = append(p.idle, pc)
	poolConnectionsIdle.WithLabelValues(p.address).Set(float64(len(p.idle)))
}

// Discard closes a connection without returning it to the pool.
func (p *BackendPool) Discard(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if pc, ok := p.inUse[conn]; ok {
		delete(p.inUse, conn)
		p.closeConn(pc)
		poolConnectionsInUse.WithLabelValues(p.address).Set(float64(len(p.inUse)))
	} else {
		conn.Close()
	}
}

// Close closes the pool and all connections.
func (p *BackendPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true

	// Close done channel to signal maintenance goroutine.
	close(p.done)

	// Close all idle connections.
	for _, pc := range p.idle {
		p.closeConn(pc)
	}
	p.idle = nil

	// Close all in-use connections.
	for _, pc := range p.inUse {
		p.closeConn(pc)
	}
	p.inUse = nil

	// Wake up all waiters.
	for _, w := range p.waiters {
		close(w)
	}
	p.waiters = nil

	p.mu.Unlock()

	// Wait for maintenance goroutine.
	p.wg.Wait()

	// Clear metrics.
	poolConnectionsIdle.DeleteLabelValues(p.address)
	poolConnectionsInUse.DeleteLabelValues(p.address)
}

// Stats returns pool statistics.
func (p *BackendPool) Stats() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	return map[string]any{
		"address":      p.address,
		"idle":         len(p.idle),
		"in_use":       len(p.inUse),
		"total":        atomic.LoadInt32(&p.totalConns),
		"max_size":     p.config.MaxPoolSize,
		"waiters":      len(p.waiters),
		"closed":       p.closed,
	}
}

func (p *BackendPool) dial(ctx context.Context) (net.Conn, error) {
	timeout := p.dialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}

	var d net.Dialer
	d.Timeout = timeout

	conn, err := d.DialContext(ctx, "tcp", p.address)
	if err != nil {
		return nil, err
	}

	// Enable TCP keepalive.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	return conn, nil
}

func (p *BackendPool) isConnectionHealthy(pc *pooledConn) bool {
	now := time.Now()

	// Check max lifetime.
	if p.config.MaxConnLifetime > 0 && now.Sub(pc.createdAt) > p.config.MaxConnLifetime {
		p.logger.Debug("connection exceeded max lifetime",
			"created_at", pc.createdAt,
			"max_lifetime", p.config.MaxConnLifetime,
		)
		return false
	}

	// Check idle timeout.
	if p.config.IdleTimeout > 0 && now.Sub(pc.lastUsedAt) > p.config.IdleTimeout {
		p.logger.Debug("connection exceeded idle timeout",
			"last_used", pc.lastUsedAt,
			"idle_timeout", p.config.IdleTimeout,
		)
		return false
	}

	return true
}

func (p *BackendPool) closeConn(pc *pooledConn) {
	pc.conn.Close()
	atomic.AddInt32(&p.totalConns, -1)
}

func (p *BackendPool) maintenance() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.config.HealthCheckPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.cleanupIdleConnections()
			p.ensureMinIdleConnections()
		}
	}
}

func (p *BackendPool) cleanupIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	now := time.Now()
	newIdle := make([]*pooledConn, 0, len(p.idle))

	for _, pc := range p.idle {
		if p.config.IdleTimeout > 0 && now.Sub(pc.lastUsedAt) > p.config.IdleTimeout {
			p.closeConn(pc)
			continue
		}
		if p.config.MaxConnLifetime > 0 && now.Sub(pc.createdAt) > p.config.MaxConnLifetime {
			p.closeConn(pc)
			continue
		}
		newIdle = append(newIdle, pc)
	}

	p.idle = newIdle
	poolConnectionsIdle.WithLabelValues(p.address).Set(float64(len(p.idle)))
}

func (p *BackendPool) ensureMinIdleConnections() {
	p.mu.Lock()
	currentIdle := len(p.idle)
	currentTotal := int(atomic.LoadInt32(&p.totalConns))
	needed := p.config.MinIdleConns - currentIdle
	canCreate := p.config.MaxPoolSize - currentTotal

	if needed <= 0 || canCreate <= 0 {
		p.mu.Unlock()
		return
	}

	if needed > canCreate {
		needed = canCreate
	}
	p.mu.Unlock()

	// Create connections outside the lock.
	for i := 0; i < needed; i++ {
		p.mu.Lock()
		if p.closed || int(atomic.LoadInt32(&p.totalConns)) >= p.config.MaxPoolSize {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), p.dialTimeout)
		conn, err := p.dial(ctx)
		cancel()

		if err != nil {
			p.logger.Debug("failed to create idle connection", "error", err)
			continue
		}

		atomic.AddInt32(&p.totalConns, 1)
		pc := &pooledConn{
			conn:        conn,
			createdAt:   time.Now(),
			lastUsedAt:  time.Now(),
			backendAddr: p.address,
		}

		p.mu.Lock()
		if p.closed {
			p.closeConn(pc)
			p.mu.Unlock()
			return
		}
		p.idle = append(p.idle, pc)
		poolConnectionsIdle.WithLabelValues(p.address).Set(float64(len(p.idle)))
		p.mu.Unlock()
	}
}

func (p *BackendPool) warmUp() {
	p.ensureMinIdleConnections()
}
