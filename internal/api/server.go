package api

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jrumley/pgbastion/internal/cluster"
	"github.com/jrumley/pgbastion/internal/consensus"
	"github.com/jrumley/pgbastion/internal/proxy"
	"github.com/jrumley/pgbastion/internal/vip"
)

// rateLimiter implements a simple token bucket rate limiter.
type rateLimiter struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func newRateLimiter(maxTokens, tokensPerSecond float64) *rateLimiter {
	return &rateLimiter{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: tokensPerSecond,
		lastRefill: time.Now(),
	}
}

func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(r.lastRefill).Seconds()
	r.tokens += elapsed * r.refillRate
	if r.tokens > r.maxTokens {
		r.tokens = r.maxTokens
	}
	r.lastRefill = now

	if r.tokens >= 1 {
		r.tokens--
		return true
	}
	return false
}

// rateLimitMiddleware returns middleware that rate limits requests.
func rateLimitMiddleware(limiter *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware returns middleware that validates Bearer token for protected endpoints.
// If authToken is empty, authentication is disabled (not recommended for production).
func authMiddleware(authToken string, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for read-only endpoints.
		if !requiresAuth(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Get request ID from response header (set by requestLogger middleware).
		requestID := w.Header().Get("X-Request-ID")

		// If no auth token configured, allow request but log warning.
		if authToken == "" {
			logger.Warn("mutation endpoint accessed without auth configured",
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
			)
			next.ServeHTTP(w, r)
			return
		}

		// Validate Bearer token.
		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "authorization required",
			})
			return
		}

		const prefix = "Bearer "
		if len(auth) < len(prefix) || auth[:len(prefix)] != prefix {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "invalid authorization format, expected Bearer token",
			})
			return
		}

		token := auth[len(prefix):]
		if token != authToken {
			logger.Warn("invalid auth token",
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
			)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "invalid token",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requiresAuth returns true if the endpoint requires authentication.
func requiresAuth(method, path string) bool {
	// All GET requests are read-only and don't require auth.
	if method == http.MethodGet {
		return false
	}

	// POST/PATCH/DELETE endpoints that mutate state require auth.
	protectedPaths := []string{
		"/raft/add-peer",
		"/raft/remove-peer",
		"/cluster/failover",
		"/cluster/switchover",
		"/cluster/reinitialize",
		"/cluster/pause",
		"/cluster/resume",
		"/cluster/postgresql",
	}

	for _, p := range protectedPaths {
		if path == p {
			return true
		}
	}

	return false
}

// Server is the REST API and metrics server.
type Server struct {
	httpServer      *http.Server
	metricsServer   *http.Server
	shutdownTimeout time.Duration
	tlsCert         string
	tlsKey          string
	logger          *slog.Logger
	authToken       string // stored for hot-reload
}

// SetAuthToken updates the auth token (for hot-reload support).
func (s *Server) SetAuthToken(token string) {
	s.authToken = token
}

// NewServer creates a new API server. raftStore/clusterManager/healthChecker/replication may be nil.
func NewServer(
	apiPort, metricsPort int,
	bindAddress string,
	authToken string,
	shutdownTimeout time.Duration,
	tlsCert, tlsKey string,
	nodeName string,
	stateSource proxy.ClusterStateSource,
	proxyInstance *proxy.Proxy,
	vipManager *vip.Manager,
	raftStore *consensus.Store,
	clusterManager *cluster.Manager,
	healthChecker *cluster.HealthChecker,
	replication *cluster.ReplicationManager,
	progressTracker *cluster.ProgressTracker,
	configApplier *cluster.ConfigApplier,
	logger *slog.Logger,
) *Server {
	h := &handlers{
		stateSource:     stateSource,
		proxyInstance:   proxyInstance,
		vipManager:      vipManager,
		raftStore:       raftStore,
		clusterManager:  clusterManager,
		healthChecker:   healthChecker,
		replication:     replication,
		progressTracker: progressTracker,
		configApplier:   configApplier,
		nodeName:        nodeName,
	}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/health", methodGuard(http.MethodGet, h.health))
	apiMux.HandleFunc("/cluster", methodGuard(http.MethodGet, h.cluster))
	apiMux.HandleFunc("/connections", methodGuard(http.MethodGet, h.connections))
	apiMux.HandleFunc("/reload", methodGuard(http.MethodPost, h.reload))

	// Raft endpoints (return 404 when raftStore is nil).
	apiMux.HandleFunc("/raft/state", methodGuard(http.MethodGet, h.raftState))
	apiMux.HandleFunc("/raft/peers", methodGuard(http.MethodGet, h.raftPeers))
	apiMux.HandleFunc("/raft/add-peer", methodGuard(http.MethodPost, h.raftAddPeer))
	apiMux.HandleFunc("/raft/remove-peer", methodGuard(http.MethodPost, h.raftRemovePeer))
	apiMux.HandleFunc("/raft/directives", methodGuard(http.MethodGet, h.raftDirectives))
	apiMux.HandleFunc("/raft/directives/", methodGuard(http.MethodGet, h.raftDirective))

	// Phase three: cluster management endpoints (return 404 when clusterManager is nil).
	apiMux.HandleFunc("/cluster/state", methodGuard(http.MethodGet, h.clusterState))
	apiMux.HandleFunc("/cluster/replication", methodGuard(http.MethodGet, h.clusterReplication))
	apiMux.HandleFunc("/cluster/health/history", methodGuard(http.MethodGet, h.healthHistory))
	apiMux.HandleFunc("/cluster/failover", methodGuard(http.MethodPost, h.clusterFailover))
	apiMux.HandleFunc("/cluster/switchover", methodGuard(http.MethodPost, h.clusterSwitchover))
	apiMux.HandleFunc("/cluster/reinitialize", methodGuard(http.MethodPost, h.clusterReinitialize))
	apiMux.HandleFunc("/cluster/config", methodGuard(http.MethodGet, h.clusterConfig))
	apiMux.HandleFunc("/cluster/pause", methodGuard(http.MethodPost, h.clusterPause))
	apiMux.HandleFunc("/cluster/resume", methodGuard(http.MethodPost, h.clusterResume))
	apiMux.HandleFunc("/cluster/progress", methodGuard(http.MethodGet, h.clusterProgress))

	// Convenience aliases for operational endpoints.
	apiMux.HandleFunc("/directives", methodGuard(http.MethodGet, h.raftDirectives))
	apiMux.HandleFunc("/members", methodGuard(http.MethodGet, h.raftPeers))
	apiMux.HandleFunc("/nodes", methodGuard(http.MethodGet, h.nodes))

	// PostgreSQL configuration endpoints.
	apiMux.HandleFunc("/cluster/postgresql", h.postgresqlConfigRouter)

	// Server-Sent Events for real-time updates.
	apiMux.HandleFunc("/events", h.events)

	// Patroni-compatible endpoints for migration/tool compatibility.
	apiMux.HandleFunc("/patroni", methodGuard(http.MethodGet, h.patroniPatroni))
	apiMux.HandleFunc("/patroni/", methodGuard(http.MethodGet, h.patroniRoot)) // Catch-all
	apiMux.HandleFunc("/primary", methodGuard(http.MethodGet, h.patroniPrimary))
	apiMux.HandleFunc("/master", methodGuard(http.MethodGet, h.patroniPrimary)) // Alias
	apiMux.HandleFunc("/replica", methodGuard(http.MethodGet, h.patroniReplica))
	apiMux.HandleFunc("/read-write", methodGuard(http.MethodGet, h.patroniReadWrite))
	apiMux.HandleFunc("/read-only", methodGuard(http.MethodGet, h.patroniReadOnly))
	apiMux.HandleFunc("/leader", methodGuard(http.MethodGet, h.patroniLeader))
	apiMux.HandleFunc("/config", methodGuard(http.MethodGet, h.patroniConfig))
	apiMux.HandleFunc("/history", methodGuard(http.MethodGet, h.patroniHistory))

	// Web UI dashboard (embedded static files).
	uiSubFS, err := fs.Sub(uiFS, "ui")
	if err == nil {
		apiMux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(uiSubFS))))
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())

	apiLogger := logger.With("component", "api-server")

	// Apply middleware chain: logging -> rate limiting -> auth.
	limiter := newRateLimiter(10, 5)
	rateLimitedMux := rateLimitMiddleware(limiter, apiMux)
	authedMux := authMiddleware(authToken, apiLogger, rateLimitedMux)
	loggedAPIMux := requestLogger(apiLogger, authedMux)

	return &Server{
		httpServer: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", bindAddress, apiPort),
			Handler: loggedAPIMux,
		},
		metricsServer: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", bindAddress, metricsPort),
			Handler: metricsMux,
		},
		shutdownTimeout: shutdownTimeout,
		tlsCert:         tlsCert,
		tlsKey:          tlsKey,
		logger:          apiLogger,
		authToken:       authToken,
	}
}

// Run starts both the API and metrics HTTP servers. Blocks until context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	go func() {
		if s.tlsCert != "" && s.tlsKey != "" {
			s.logger.Info("API server starting with TLS", "addr", s.httpServer.Addr)
			if err := s.httpServer.ListenAndServeTLS(s.tlsCert, s.tlsKey); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("API server (TLS): %w", err)
			}
		} else {
			s.logger.Info("API server starting", "addr", s.httpServer.Addr)
			if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("API server: %w", err)
			}
		}
	}()

	go func() {
		s.logger.Info("metrics server starting", "addr", s.metricsServer.Addr)
		if err := s.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		s.httpServer.Shutdown(shutdownCtx)
		s.metricsServer.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
