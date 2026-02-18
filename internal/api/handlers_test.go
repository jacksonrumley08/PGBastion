package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jrumley/pgbastion/internal/proxy"
	"github.com/jrumley/pgbastion/internal/vip"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// mockClusterState implements proxy.ClusterStateSource for testing.
type mockClusterState struct {
	primary    *proxy.NodeState
	replicas   []*proxy.NodeState
	splitBrain bool
}

func (m *mockClusterState) GetPrimary() *proxy.NodeState    { return m.primary }
func (m *mockClusterState) GetReplicas() []*proxy.NodeState { return m.replicas }
func (m *mockClusterState) IsSplitBrain() bool              { return m.splitBrain }

type testSetup struct {
	mux   *http.ServeMux
	state *mockClusterState
}

func newTestSetup(nodeName string) *testSetup {
	logger := testLogger()

	state := &mockClusterState{}
	router := proxy.NewRouter(state, logger)
	proxyInstance := proxy.NewProxy(15432, 15433, 30*time.Second, 5*time.Second, router, logger)
	vipManager := vip.NewManager("", "", nodeName, 10*time.Second, nil, logger)

	h := &handlers{
		stateSource:   state,
		proxyInstance: proxyInstance,
		vipManager:    vipManager,
		nodeName:      nodeName,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", methodGuard(http.MethodGet, h.health))
	mux.HandleFunc("/cluster", methodGuard(http.MethodGet, h.cluster))
	mux.HandleFunc("/connections", methodGuard(http.MethodGet, h.connections))
	mux.HandleFunc("/reload", methodGuard(http.MethodPost, h.reload))

	return &testSetup{mux: mux, state: state}
}

func TestHealth_NoState(t *testing.T) {
	ts := newTestSetup("node1")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["node"] != "node1" {
		t.Errorf("expected node=node1, got %v", resp["node"])
	}
	if resp["role"] != "unknown" {
		t.Errorf("expected role=unknown, got %v", resp["role"])
	}
	if resp["proxy_healthy"] != false {
		t.Errorf("expected proxy_healthy=false, got %v", resp["proxy_healthy"])
	}
}

func TestHealth_WithPrimary(t *testing.T) {
	ts := newTestSetup("node1")

	// Set cluster state directly.
	ts.state.primary = &proxy.NodeState{Name: "node1", Host: "10.0.1.1", Port: 5432}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["role"] != "primary" {
		t.Errorf("expected role=primary, got %v", resp["role"])
	}
	if resp["proxy_healthy"] != true {
		t.Errorf("expected proxy_healthy=true, got %v", resp["proxy_healthy"])
	}
}

func TestHealth_ReplicaRole(t *testing.T) {
	ts := newTestSetup("node2")

	ts.state.primary = &proxy.NodeState{Name: "node1", Host: "10.0.1.1", Port: 5432}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["role"] != "replica" {
		t.Errorf("expected role=replica, got %v", resp["role"])
	}
}

func TestHealth_SplitBrain(t *testing.T) {
	ts := newTestSetup("node1")

	ts.state.splitBrain = true

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 during split-brain, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["split_brain"] != true {
		t.Errorf("expected split_brain=true")
	}
	if resp["status"] != "split_brain" {
		t.Errorf("expected status=split_brain, got %v", resp["status"])
	}
}

func TestHealth_MethodNotAllowed(t *testing.T) {
	ts := newTestSetup("node1")

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestCluster_Endpoint(t *testing.T) {
	ts := newTestSetup("node1")

	ts.state.primary = &proxy.NodeState{Name: "node1", Host: "10.0.1.1", Port: 5432}
	ts.state.replicas = []*proxy.NodeState{
		{Name: "node2", Host: "10.0.1.2", Port: 5432},
	}

	req := httptest.NewRequest(http.MethodGet, "/cluster", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	rt, ok := resp["routing_table"].(map[string]any)
	if !ok {
		t.Fatal("missing routing_table in response")
	}
	if rt["primary"] != "10.0.1.1:5432" {
		t.Errorf("expected primary=10.0.1.1:5432, got %v", rt["primary"])
	}
}

func TestCluster_MethodNotAllowed(t *testing.T) {
	ts := newTestSetup("node1")

	req := httptest.NewRequest(http.MethodDelete, "/cluster", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestConnections_Endpoint(t *testing.T) {
	ts := newTestSetup("node1")

	req := httptest.NewRequest(http.MethodGet, "/connections", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["active_connections"] != float64(0) {
		t.Errorf("expected 0 active connections, got %v", resp["active_connections"])
	}
}

func TestReload_Endpoint(t *testing.T) {
	ts := newTestSetup("node1")

	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["status"] != "state is automatically synchronized via Raft" {
		t.Errorf("expected Raft sync message, got %v", resp["status"])
	}
}

func TestReload_GetNotAllowed(t *testing.T) {
	ts := newTestSetup("node1")

	req := httptest.NewRequest(http.MethodGet, "/reload", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestContentType_JSON(t *testing.T) {
	ts := newTestSetup("node1")

	for _, path := range []string{"/health", "/cluster", "/connections"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		ts.mux.ServeHTTP(w, req)

		ct := w.Header().Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("%s: expected Content-Type=application/json, got %s", path, ct)
		}
	}
}

func TestRaftDirectives_NotConfigured(t *testing.T) {
	ts := newTestSetup("node1")

	// Add the directive routes to the test mux (without raft store).
	h := &handlers{nodeName: "node1"}
	ts.mux.HandleFunc("/raft/directives", methodGuard(http.MethodGet, h.raftDirectives))

	req := httptest.NewRequest(http.MethodGet, "/raft/directives", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when raft not configured, got %d", w.Code)
	}
}

func TestRaftDirective_NotConfigured(t *testing.T) {
	ts := newTestSetup("node1")

	h := &handlers{nodeName: "node1"}
	ts.mux.HandleFunc("/raft/directives/", methodGuard(http.MethodGet, h.raftDirective))

	req := httptest.NewRequest(http.MethodGet, "/raft/directives/some-id", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when raft not configured, got %d", w.Code)
	}
}
