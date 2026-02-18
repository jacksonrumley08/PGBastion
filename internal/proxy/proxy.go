package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	proxyConnectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_proxy_connections_total",
		Help: "Total number of proxy connections accepted",
	}, []string{"port_type"})

	proxyConnectionsActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgbastion_proxy_connections_active",
		Help: "Number of active proxy connections",
	}, []string{"port_type"})

	proxyRoutingErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_proxy_routing_errors_total",
		Help: "Total number of routing errors",
	}, []string{"port_type", "reason"})
)

func init() {
	// Pre-initialize metrics with all label combinations so they appear in /metrics.
	proxyConnectionsTotal.WithLabelValues("primary")
	proxyConnectionsTotal.WithLabelValues("replica")
	proxyConnectionsActive.WithLabelValues("primary")
	proxyConnectionsActive.WithLabelValues("replica")
}

// Proxy listens for incoming PostgreSQL client connections and routes them
// to the appropriate backend based on the Router's routing table.
type Proxy struct {
	primaryPort    int
	replicaPort    int
	drainTimeout   time.Duration
	connectTimeout time.Duration
	router         *Router
	tracker        *ConnectionTracker
	logger         *slog.Logger
}

// NewProxy creates a new TCP proxy.
func NewProxy(primaryPort, replicaPort int, drainTimeout, connectTimeout time.Duration, router *Router, logger *slog.Logger) *Proxy {
	return &Proxy{
		primaryPort:    primaryPort,
		replicaPort:    replicaPort,
		drainTimeout:   drainTimeout,
		connectTimeout: connectTimeout,
		router:         router,
		tracker:        NewConnectionTracker(logger),
		logger:         logger.With("component", "proxy"),
	}
}

// Tracker returns the connection tracker for observability.
func (p *Proxy) Tracker() *ConnectionTracker {
	return p.tracker
}

// Router returns the router for observability.
func (p *Proxy) Router() *Router {
	return p.router
}

// Run starts both primary and replica listeners. Blocks until context is cancelled.
func (p *Proxy) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	go func() {
		errCh <- p.listen(ctx, p.primaryPort, "primary", p.router.PrimaryAddr)
	}()

	go func() {
		errCh <- p.listen(ctx, p.replicaPort, "replica", p.router.ReplicaAddr)
	}()

	select {
	case <-ctx.Done():
		p.logger.Info("proxy shutting down, draining connections", "timeout", p.drainTimeout)
		p.tracker.DrainAll(p.drainTimeout)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (p *Proxy) listen(ctx context.Context, port int, portType string, addrFn func() string) error {
	addr := fmt.Sprintf(":%d", port)
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer ln.Close()

	p.logger.Info("proxy listener started", "port", port, "type", portType)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		clientConn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				p.logger.Error("accept error", "error", err, "type", portType)
				continue
			}
		}

		// Enable TCP keepalive on client connections.
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
		}

		proxyConnectionsTotal.WithLabelValues(portType).Inc()

		go p.handleConn(ctx, clientConn, portType, addrFn)
	}
}

func (p *Proxy) handleConn(ctx context.Context, clientConn net.Conn, portType string, addrFn func() string) {
	backendAddr := addrFn()
	if backendAddr == "" {
		p.logger.Warn("no backend available, closing client connection",
			"client", clientConn.RemoteAddr(),
			"type", portType,
		)
		proxyRoutingErrors.WithLabelValues(portType, "no_backend").Inc()
		clientConn.Close()
		return
	}

	backendConn, err := net.DialTimeout("tcp", backendAddr, p.connectTimeout)
	if err == nil {
		// Enable TCP keepalive on the backend connection to detect dead peers.
		if tc, ok := backendConn.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
		}
	}
	if err != nil {
		p.logger.Error("failed to connect to backend",
			"backend", backendAddr,
			"error", err,
			"type", portType,
		)
		proxyRoutingErrors.WithLabelValues(portType, "backend_connect_failed").Inc()
		clientConn.Close()
		return
	}

	connCtx, cancel := context.WithCancel(ctx)
	id := p.tracker.Add(clientConn, backendConn, cancel)

	proxyConnectionsActive.WithLabelValues(portType).Inc()

	p.logger.Debug("connection established",
		"id", id,
		"client", clientConn.RemoteAddr(),
		"backend", backendAddr,
		"type", portType,
	)

	stats := &ConnStats{
		StartTime:   time.Now(),
		ClientAddr:  clientConn.RemoteAddr().String(),
		BackendAddr: backendAddr,
	}

	// Wait for either pipe completion or context cancellation.
	done := make(chan struct{})
	go func() {
		Pipe(clientConn, backendConn, stats, p.logger)
		close(done)
	}()

	select {
	case <-done:
	case <-connCtx.Done():
	}

	clientConn.Close()
	backendConn.Close()
	p.tracker.Remove(id)
	cancel()

	proxyConnectionsActive.WithLabelValues(portType).Dec()

	p.logger.Debug("connection closed",
		"id", id,
		"client", stats.ClientAddr,
		"backend", stats.BackendAddr,
		"type", portType,
		"bytes_sent", stats.BytesSent.Load(),
		"bytes_received", stats.BytesReceived.Load(),
	)
}
