package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/jrumley/pgbastion/internal/cluster"
	"github.com/jrumley/pgbastion/internal/consensus"
	"github.com/jrumley/pgbastion/internal/proxy"
	"github.com/jrumley/pgbastion/internal/vip"
)

var (
	apiRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pgbastion_api_request_duration_seconds",
		Help:    "Duration of API requests in seconds",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms to ~1s
	}, []string{"method", "path", "status"})

	apiRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_api_requests_total",
		Help: "Total number of API requests",
	}, []string{"method", "path", "status"})
)

// requestIDHeader is the header name for request correlation IDs.
const requestIDHeader = "X-Request-ID"

// generateRequestID creates a random 16-character hex string for request tracing.
func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID if crypto/rand fails.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// methodGuard wraps a handler and enforces the required HTTP method.
func methodGuard(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// requestLogger is middleware that logs every API request with a correlation ID.
func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get or generate request ID for correlation.
		requestID := r.Header.Get(requestIDHeader)
		if requestID == "" {
			requestID = generateRequestID()
		}

		// Add request ID to response headers for client correlation.
		w.Header().Set(requestIDHeader, requestID)

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		duration := time.Since(start)
		statusStr := fmt.Sprintf("%d", sw.status)

		// Record metrics.
		apiRequestDuration.WithLabelValues(r.Method, r.URL.Path, statusStr).Observe(duration.Seconds())
		apiRequestsTotal.WithLabelValues(r.Method, r.URL.Path, statusStr).Inc()

		logger.Debug("api request",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"remote", r.RemoteAddr,
			"duration", duration,
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

type handlers struct {
	stateSource     proxy.ClusterStateSource
	proxyInstance   *proxy.Proxy
	vipManager      *vip.Manager
	raftStore       *consensus.Store
	clusterManager  *cluster.Manager
	healthChecker   *cluster.HealthChecker
	replication     *cluster.ReplicationManager
	progressTracker *cluster.ProgressTracker
	nodeName        string
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	primary := h.stateSource.GetPrimary()
	replicas := h.stateSource.GetReplicas()
	splitBrain := h.stateSource.IsSplitBrain()

	role := "unknown"
	if primary != nil && primary.Name == h.nodeName {
		role = "primary"
	} else if primary != nil {
		role = "replica"
	}

	// Proxy is healthy if we have a known primary or replicas to route to.
	proxyHealthy := primary != nil || len(replicas) > 0

	status := "ok"
	statusCode := http.StatusOK
	if splitBrain {
		status = "split_brain"
		statusCode = http.StatusServiceUnavailable
	} else if !proxyHealthy {
		status = "no_backends"
		statusCode = http.StatusServiceUnavailable
	}

	resp := map[string]any{
		"status":        status,
		"node":          h.nodeName,
		"role":          role,
		"split_brain":   splitBrain,
		"vip_owner":     h.vipManager.IsOwner(),
		"proxy_healthy": proxyHealthy,
	}

	writeJSON(w, statusCode, resp)
}

func (h *handlers) cluster(w http.ResponseWriter, r *http.Request) {
	table := h.proxyInstance.Router().RoutingTable()

	resp := map[string]any{
		"routing_table": table,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) connections(w http.ResponseWriter, r *http.Request) {
	tracker := h.proxyInstance.Tracker()

	resp := map[string]any{
		"active_connections": tracker.Count(),
		"by_backend":         tracker.ConnectionsByBackend(),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) reload(w http.ResponseWriter, r *http.Request) {
	// State is automatically updated via Raft consensus - no manual reload needed.
	writeJSON(w, http.StatusOK, map[string]string{"status": "state is automatically synchronized via Raft"})
}

// Raft API handlers.

func (h *handlers) raftState(w http.ResponseWriter, r *http.Request) {
	if h.raftStore == nil {
		http.Error(w, "raft not configured", http.StatusNotFound)
		return
	}

	stats := h.raftStore.Stats()
	resp := map[string]any{
		"state":        h.raftStore.RaftState(),
		"is_leader":    h.raftStore.IsLeader(),
		"leader_addr":  h.raftStore.LeaderAddr(),
		"commit_index": stats["commit_index"],
		"last_log_index": stats["last_log_index"],
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) raftPeers(w http.ResponseWriter, r *http.Request) {
	if h.raftStore == nil {
		http.Error(w, "raft not configured", http.StatusNotFound)
		return
	}

	peers, err := h.raftStore.Peers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	type peerInfo struct {
		ID       string `json:"id"`
		Address  string `json:"address"`
		Suffrage string `json:"suffrage"`
	}

	var result []peerInfo
	for _, p := range peers {
		result = append(result, peerInfo{
			ID:       string(p.ID),
			Address:  string(p.Address),
			Suffrage: p.Suffrage.String(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"peers": result})
}

func (h *handlers) raftAddPeer(w http.ResponseWriter, r *http.Request) {
	if h.raftStore == nil {
		http.Error(w, "raft not configured", http.StatusNotFound)
		return
	}

	var req struct {
		ID      string `json:"id"`
		Address string `json:"address"`
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.ID == "" || req.Address == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id and address are required"})
		return
	}

	if err := h.raftStore.AddPeer(req.ID, req.Address); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "peer added"})
}

func (h *handlers) raftRemovePeer(w http.ResponseWriter, r *http.Request) {
	if h.raftStore == nil {
		http.Error(w, "raft not configured", http.StatusNotFound)
		return
	}

	var req struct {
		ID string `json:"id"`
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}

	if err := h.raftStore.RemovePeer(req.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "peer removed"})
}

// Phase three: cluster management endpoints.

func (h *handlers) clusterState(w http.ResponseWriter, r *http.Request) {
	if h.clusterManager == nil {
		http.Error(w, "cluster manager not configured", http.StatusNotFound)
		return
	}

	resp := map[string]any{
		"node":   h.nodeName,
		"state":  string(h.clusterManager.State()),
		"paused": h.clusterManager.IsPaused(),
	}

	if h.raftStore != nil {
		resp["is_leader"] = h.raftStore.IsLeader()
		nodes := h.raftStore.FSM().GetAllNodes()
		nodeStates := make(map[string]any)
		for name, n := range nodes {
			nodeStates[name] = map[string]any{
				"role":      n.Role,
				"state":     n.State,
				"lag":       n.Lag,
				"last_seen": n.LastSeen,
			}
		}
		resp["nodes"] = nodeStates
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) clusterReplication(w http.ResponseWriter, r *http.Request) {
	if h.healthChecker == nil {
		http.Error(w, "health checker not configured", http.StatusNotFound)
		return
	}

	status := h.healthChecker.Status()
	resp := map[string]any{
		"is_in_recovery":    status.IsInRecovery,
		"replication_lag":   status.ReplicationLag,
		"replicas":          status.Replicas,
		"replication_slots": status.ReplicationSlots,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) healthHistory(w http.ResponseWriter, r *http.Request) {
	if h.healthChecker == nil {
		http.Error(w, "health checker not configured", http.StatusNotFound)
		return
	}

	history := h.healthChecker.History()
	if history == nil {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}, "stats": map[string]any{}})
		return
	}

	// Check for limit parameter.
	limitStr := r.URL.Query().Get("limit")
	var entries []cluster.HealthCheckEntry
	if limitStr != "" {
		var limit int
		if _, err := fmt.Sscanf(limitStr, "%d", &limit); err == nil && limit > 0 {
			entries = history.GetRecent(limit)
		} else {
			entries = history.GetRecent(20) // Default to 20 most recent
		}
	} else {
		entries = history.GetRecent(20)
	}

	// Convert entries to JSON-friendly format.
	type entryResp struct {
		Timestamp  string `json:"timestamp"`
		DurationMs int64  `json:"duration_ms"`
		Healthy    bool   `json:"healthy"`
		Error      string `json:"error,omitempty"`
	}

	var respEntries []entryResp
	for _, e := range entries {
		re := entryResp{
			Timestamp:  e.Timestamp.Format(time.RFC3339),
			DurationMs: e.Duration.Milliseconds(),
			Healthy:    e.Status.Healthy,
		}
		if e.Status.Error != "" {
			re.Error = e.Status.Error
		}
		respEntries = append(respEntries, re)
	}

	resp := map[string]any{
		"entries": respEntries,
		"stats":   history.Stats(),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) clusterFailover(w http.ResponseWriter, r *http.Request) {
	if h.clusterManager == nil {
		http.Error(w, "cluster manager not configured", http.StatusNotFound)
		return
	}

	var req struct {
		To string `json:"to"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.To == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "\"to\" field is required"})
		return
	}

	if err := h.clusterManager.ManualFailover(r.Context(), req.To); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "failover initiated"})
}

func (h *handlers) clusterSwitchover(w http.ResponseWriter, r *http.Request) {
	if h.clusterManager == nil {
		http.Error(w, "cluster manager not configured", http.StatusNotFound)
		return
	}

	var req struct {
		To string `json:"to"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.To == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "\"to\" field is required"})
		return
	}

	if err := h.clusterManager.ManualSwitchover(r.Context(), req.To); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "switchover initiated"})
}

func (h *handlers) clusterReinitialize(w http.ResponseWriter, r *http.Request) {
	if h.clusterManager == nil || h.replication == nil {
		http.Error(w, "cluster manager not configured", http.StatusNotFound)
		return
	}

	if h.raftStore == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "raft not configured"})
		return
	}

	primary := h.raftStore.FSM().GetPrimary()
	if primary == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no primary known"})
		return
	}

	if err := h.replication.Reinitialize(r.Context(), primary.Host); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "reinitialization complete"})
}

func (h *handlers) clusterConfig(w http.ResponseWriter, r *http.Request) {
	if h.healthChecker == nil {
		http.Error(w, "cluster manager not configured", http.StatusNotFound)
		return
	}

	status := h.healthChecker.Status()
	resp := map[string]any{
		"healthy":     status.Healthy,
		"is_recovery": status.IsInRecovery,
		"last_check":  status.LastCheck,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) clusterPause(w http.ResponseWriter, r *http.Request) {
	if h.clusterManager == nil {
		http.Error(w, "cluster manager not configured", http.StatusNotFound)
		return
	}
	h.clusterManager.Pause()
	writeJSON(w, http.StatusOK, map[string]string{"status": "automatic failover paused"})
}

func (h *handlers) clusterResume(w http.ResponseWriter, r *http.Request) {
	if h.clusterManager == nil {
		http.Error(w, "cluster manager not configured", http.StatusNotFound)
		return
	}
	h.clusterManager.Resume()
	writeJSON(w, http.StatusOK, map[string]string{"status": "automatic failover resumed"})
}

func (h *handlers) clusterProgress(w http.ResponseWriter, r *http.Request) {
	if h.progressTracker == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"rewind": map[string]any{"phase": "idle"},
		})
		return
	}

	progress := h.progressTracker.GetRewindProgress()
	resp := map[string]any{
		"rewind": map[string]any{
			"phase":        string(progress.Phase),
			"percent":      progress.Percent,
			"bytes_total":  progress.BytesTotal,
			"bytes_copied": progress.BytesCopied,
			"files_total":  progress.FilesTotal,
			"files_copied": progress.FilesCopied,
		},
	}

	if !progress.StartTime.IsZero() {
		resp["rewind"].(map[string]any)["start_time"] = progress.StartTime.Format(time.RFC3339)
		resp["rewind"].(map[string]any)["elapsed_seconds"] = time.Since(progress.StartTime).Seconds()
	}
	if progress.Error != "" {
		resp["rewind"].(map[string]any)["error"] = progress.Error
	}
	if len(progress.Output) > 0 {
		// Only return last 10 lines to keep response small.
		outputLines := progress.Output
		if len(outputLines) > 10 {
			outputLines = outputLines[len(outputLines)-10:]
		}
		resp["rewind"].(map[string]any)["recent_output"] = outputLines
	}

	writeJSON(w, http.StatusOK, resp)
}

// raftDirectives returns all directives, optionally filtered by node and/or status.
func (h *handlers) raftDirectives(w http.ResponseWriter, r *http.Request) {
	if h.raftStore == nil {
		http.Error(w, "raft not configured", http.StatusNotFound)
		return
	}

	nodeFilter := r.URL.Query().Get("node")
	statusFilter := r.URL.Query().Get("status")

	allDirectives := h.raftStore.GetAllDirectives()

	type directiveInfo struct {
		ID          string            `json:"id"`
		Type        string            `json:"type"`
		TargetNode  string            `json:"target_node"`
		Params      map[string]string `json:"params,omitempty"`
		Status      string            `json:"status"`
		CreatedAt   string            `json:"created_at"`
		CompletedAt string            `json:"completed_at,omitempty"`
		Error       string            `json:"error,omitempty"`
	}

	var result []directiveInfo
	for _, d := range allDirectives {
		if nodeFilter != "" && d.TargetNode != nodeFilter {
			continue
		}
		if statusFilter != "" && d.Status != statusFilter {
			continue
		}
		di := directiveInfo{
			ID:         d.ID,
			Type:       d.Type,
			TargetNode: d.TargetNode,
			Params:     d.Params,
			Status:     d.Status,
			CreatedAt:  d.CreatedAt.Format(time.RFC3339),
		}
		if !d.CompletedAt.IsZero() {
			di.CompletedAt = d.CompletedAt.Format(time.RFC3339)
		}
		if d.Error != "" {
			di.Error = d.Error
		}
		result = append(result, di)
	}

	writeJSON(w, http.StatusOK, map[string]any{"directives": result})
}

// raftDirective returns a single directive by ID.
func (h *handlers) raftDirective(w http.ResponseWriter, r *http.Request) {
	if h.raftStore == nil {
		http.Error(w, "raft not configured", http.StatusNotFound)
		return
	}

	// Extract ID from path: /raft/directives/{id}
	path := r.URL.Path
	id := path[len("/raft/directives/"):]
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "directive ID required"})
		return
	}

	d := h.raftStore.GetDirective(id)
	if d == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "directive not found"})
		return
	}

	resp := map[string]any{
		"id":          d.ID,
		"type":        d.Type,
		"target_node": d.TargetNode,
		"status":      d.Status,
		"created_at":  d.CreatedAt.Format(time.RFC3339),
	}
	if d.Params != nil {
		resp["params"] = d.Params
	}
	if !d.CompletedAt.IsZero() {
		resp["completed_at"] = d.CompletedAt.Format(time.RFC3339)
	}
	if d.Error != "" {
		resp["error"] = d.Error
	}

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Patroni-compatible API endpoints for tool compatibility.

// patroniRoot returns basic node information (Patroni-compatible /).
func (h *handlers) patroniRoot(w http.ResponseWriter, r *http.Request) {
	primary := h.stateSource.GetPrimary()

	role := "replica"
	pgState := "running"
	if h.healthChecker != nil {
		status := h.healthChecker.Status()
		if !status.Healthy {
			pgState = "unknown"
		}
		if !status.IsInRecovery {
			role = "master"
		}
	} else if primary != nil && primary.Name == h.nodeName {
		role = "master"
	}

	resp := map[string]any{
		"state":          pgState,
		"role":           role,
		"server_version": 160000, // PostgreSQL 16 placeholder
		"pgbastion": map[string]any{
			"version": "1.0.0",
			"scope":   "pgbastion",
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// patroniPatroni returns detailed Patroni-compatible info.
func (h *handlers) patroniPatroni(w http.ResponseWriter, r *http.Request) {
	primary := h.stateSource.GetPrimary()
	replicas := h.stateSource.GetReplicas()

	role := "replica"
	if h.healthChecker != nil {
		status := h.healthChecker.Status()
		if !status.IsInRecovery {
			role = "master"
		}
	} else if primary != nil && primary.Name == h.nodeName {
		role = "master"
	}

	// Build timeline from primary if available.
	var timeline int64 = 1
	if primary != nil {
		timeline = primary.Timeline
	}

	resp := map[string]any{
		"state":    "running",
		"role":     role,
		"timeline": timeline,
		"cluster_unlocked": false,
		"dcs_last_seen":    time.Now().Unix(),
		"pgbastion": map[string]any{
			"version": "1.0.0",
			"scope":   "pgbastion",
		},
		"database_system_identifier": "pgbastion",
		"pending_restart":            false,
	}

	// Add replication info if primary.
	if role == "master" && len(replicas) > 0 {
		var replicaList []map[string]any
		for _, rep := range replicas {
			replicaList = append(replicaList, map[string]any{
				"name":     rep.Name,
				"host":     rep.Host,
				"port":     rep.Port,
				"lag":      rep.Lag,
				"timeline": rep.Timeline,
			})
		}
		resp["replication"] = replicaList
	}

	writeJSON(w, http.StatusOK, resp)
}

// patroniCluster returns Patroni-compatible cluster state.
func (h *handlers) patroniCluster(w http.ResponseWriter, r *http.Request) {
	primary := h.stateSource.GetPrimary()
	replicas := h.stateSource.GetReplicas()

	var members []map[string]any

	if primary != nil {
		members = append(members, map[string]any{
			"name":     primary.Name,
			"host":     primary.Host,
			"port":     primary.Port,
			"role":     "leader",
			"state":    primary.State,
			"timeline": primary.Timeline,
			"lag":      0,
			"api_url":  fmt.Sprintf("http://%s:8080/", primary.Host),
		})
	}

	for _, rep := range replicas {
		members = append(members, map[string]any{
			"name":     rep.Name,
			"host":     rep.Host,
			"port":     rep.Port,
			"role":     "replica",
			"state":    rep.State,
			"timeline": rep.Timeline,
			"lag":      rep.Lag,
			"api_url":  fmt.Sprintf("http://%s:8080/", rep.Host),
		})
	}

	resp := map[string]any{
		"members": members,
		"scope":   "pgbastion",
	}

	writeJSON(w, http.StatusOK, resp)
}

// patroniPrimary returns 200 if this node is the primary, 503 otherwise.
func (h *handlers) patroniPrimary(w http.ResponseWriter, r *http.Request) {
	isPrimary := false
	if h.healthChecker != nil {
		status := h.healthChecker.Status()
		isPrimary = status.Healthy && !status.IsInRecovery
	} else {
		primary := h.stateSource.GetPrimary()
		isPrimary = primary != nil && primary.Name == h.nodeName
	}

	if isPrimary {
		writeJSON(w, http.StatusOK, map[string]any{
			"role":    "master",
			"state":   "running",
			"message": "this node is the primary",
		})
	} else {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"role":    "replica",
			"message": "this node is not the primary",
		})
	}
}

// patroniReplica returns 200 if this node is a replica, 503 otherwise.
func (h *handlers) patroniReplica(w http.ResponseWriter, r *http.Request) {
	isReplica := false
	if h.healthChecker != nil {
		status := h.healthChecker.Status()
		isReplica = status.Healthy && status.IsInRecovery
	} else {
		primary := h.stateSource.GetPrimary()
		isReplica = primary == nil || primary.Name != h.nodeName
	}

	if isReplica {
		writeJSON(w, http.StatusOK, map[string]any{
			"role":    "replica",
			"state":   "running",
			"message": "this node is a replica",
		})
	} else {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"role":    "master",
			"message": "this node is not a replica",
		})
	}
}

// patroniReadWrite returns 200 if accepting writes (primary), 503 otherwise.
func (h *handlers) patroniReadWrite(w http.ResponseWriter, r *http.Request) {
	h.patroniPrimary(w, r)
}

// patroniReadOnly returns 200 if accepting reads (any healthy node).
func (h *handlers) patroniReadOnly(w http.ResponseWriter, r *http.Request) {
	healthy := false
	isReplica := true
	if h.healthChecker != nil {
		status := h.healthChecker.Status()
		healthy = status.Healthy
		isReplica = status.IsInRecovery
	} else {
		primary := h.stateSource.GetPrimary()
		replicas := h.stateSource.GetReplicas()
		healthy = primary != nil || len(replicas) > 0
	}

	if healthy && isReplica {
		writeJSON(w, http.StatusOK, map[string]any{
			"role":    "replica",
			"state":   "running",
			"message": "this node is available for read-only queries",
		})
	} else {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"message": "this node is not available for read-only queries",
		})
	}
}

// patroniLeader redirects to the current leader or returns leader info.
func (h *handlers) patroniLeader(w http.ResponseWriter, r *http.Request) {
	primary := h.stateSource.GetPrimary()

	if primary == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "no leader available",
		})
		return
	}

	// Return leader info.
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    primary.Name,
		"host":    primary.Host,
		"port":    primary.Port,
		"api_url": fmt.Sprintf("http://%s:8080/", primary.Host),
	})
}

// patroniConfig returns cluster configuration (Patroni-compatible).
func (h *handlers) patroniConfig(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"ttl":                  30,
		"loop_wait":            10,
		"retry_timeout":        10,
		"maximum_lag_on_failover": 1048576,
		"postgresql": map[string]any{
			"use_pg_rewind": true,
			"use_slots":     true,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// patroniHistory returns cluster history (simplified).
func (h *handlers) patroniHistory(w http.ResponseWriter, r *http.Request) {
	// Return empty history - full implementation would require tracking failover events.
	writeJSON(w, http.StatusOK, []any{})
}
