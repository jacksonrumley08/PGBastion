package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests require the docker-compose stack to be running:
//
//	cd test/e2e && docker compose up -d
//	# wait for PostgreSQL to be ready
//	PGBASTION_E2E=1 go test ./test/e2e/ -v -count=1 -timeout 120s
//
// And the pgbastion binary must be built:
//
//	go build -o pgbastion ./cmd/pgbastion/

const (
	apiBase      = "http://127.0.0.1:18080"
	metricsBase  = "http://127.0.0.1:19090"
	proxyPrimary = "127.0.0.1:16432"
	proxyReplica = "127.0.0.1:16433"
	configPath   = "pgbastion-e2e.yaml"
	raftDataDir  = "/tmp/pgbastion-e2e-raft"
)

var pgbastionCancel context.CancelFunc

func TestMain(m *testing.M) {
	if os.Getenv("PGBASTION_E2E") != "1" {
		fmt.Println("skipping e2e tests (set PGBASTION_E2E=1 to run)")
		os.Exit(0)
	}

	// Clean up any previous Raft data.
	os.RemoveAll(raftDataDir)

	// Check that the docker-compose PostgreSQL is reachable.
	if !postgresReachable() {
		fmt.Println("SKIP: PostgreSQL not reachable at 127.0.0.1:15432")
		fmt.Println("Run: cd test/e2e && docker compose up -d && sleep 10")
		os.Exit(0)
	}

	// Start pgbastion in the background.
	ctx, cancel := context.WithCancel(context.Background())
	pgbastionCancel = cancel

	cmd := exec.CommandContext(ctx, "../../pgbastion", "start", "--config", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Printf("failed to start pgbastion: %v\n", err)
		os.Exit(1)
	}

	// Wait for the API to be ready.
	if !waitForAPI(15 * time.Second) {
		fmt.Println("pgbastion API did not become ready in time")
		cancel()
		cmd.Wait()
		os.Exit(1)
	}

	// Wait for Raft to elect a leader.
	if !waitForRaftLeader(10 * time.Second) {
		fmt.Println("pgbastion did not become Raft leader in time")
		cancel()
		cmd.Wait()
		os.Exit(1)
	}

	code := m.Run()

	cancel()
	cmd.Wait()

	// Clean up Raft data.
	os.RemoveAll(raftDataDir)

	os.Exit(code)
}

func postgresReachable() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	// Try to connect to psql via a simple TCP check.
	conn, err := client.Get("http://127.0.0.1:15432/")
	if err == nil {
		conn.Body.Close()
	}
	// PostgreSQL won't respond to HTTP, but if we get a connection refused,
	// the container isn't running. Any other error means it's up.
	return err == nil || !strings.Contains(err.Error(), "connection refused")
}

func waitForAPI(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(apiBase + "/health")
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func waitForRaftLeader(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(apiBase + "/raft/state")
		if err == nil {
			var state map[string]any
			json.NewDecoder(resp.Body).Decode(&state)
			resp.Body.Close()
			if isLeader, ok := state["is_leader"].(bool); ok && isLeader {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
	return result
}

// Test 1: Verify /health returns a valid response.
func TestE2E_HealthEndpoint(t *testing.T) {
	health := getJSON(t, apiBase+"/health")

	node, _ := health["node"].(string)
	if node != "e2e-node" {
		t.Errorf("expected node=e2e-node, got %s", node)
	}

	// VIP owner should be true since we're the Raft leader.
	vipOwner, _ := health["vip_owner"].(bool)
	t.Logf("vip_owner: %v", vipOwner)

	splitBrain, _ := health["split_brain"].(bool)
	if splitBrain {
		t.Error("unexpected split-brain in single-node cluster")
	}
}

// Test 2: Verify /cluster returns routing table.
func TestE2E_ClusterEndpoint(t *testing.T) {
	cluster := getJSON(t, apiBase+"/cluster")

	rt, ok := cluster["routing_table"].(map[string]any)
	if !ok {
		t.Fatal("missing routing_table")
	}

	// In a fresh cluster without PostgreSQL management, routing table may be empty.
	t.Logf("routing_table: %v", rt)

	splitBrain, _ := rt["split_brain"].(bool)
	if splitBrain {
		t.Error("unexpected split-brain")
	}
}

// Test 3: Verify /raft/state shows leader status.
func TestE2E_RaftState(t *testing.T) {
	state := getJSON(t, apiBase+"/raft/state")

	isLeader, _ := state["is_leader"].(bool)
	if !isLeader {
		t.Error("expected is_leader=true in single-node cluster")
	}

	raftState, _ := state["state"].(string)
	if raftState != "Leader" {
		t.Errorf("expected state=Leader, got %s", raftState)
	}

	t.Logf("raft state: %v", state)
}

// Test 4: Verify /raft/peers returns peer list.
func TestE2E_RaftPeers(t *testing.T) {
	result := getJSON(t, apiBase+"/raft/peers")

	peers, ok := result["peers"].([]any)
	if !ok {
		t.Fatal("missing peers array in response")
	}

	if len(peers) != 1 {
		t.Errorf("expected 1 peer in single-node cluster, got %d", len(peers))
	}
	t.Logf("peers: %v", peers)
}

// Test 5: Verify /raft/directives returns directive list.
func TestE2E_RaftDirectives(t *testing.T) {
	result := getJSON(t, apiBase+"/raft/directives")

	directives, ok := result["directives"]
	if !ok {
		t.Fatal("missing directives key in response")
	}

	// Fresh cluster should have no directives (nil or empty array).
	if directives != nil {
		if arr, ok := directives.([]any); ok {
			t.Logf("directives count: %d", len(arr))
		}
	} else {
		t.Logf("directives count: 0")
	}
}

// Test 6: Verify /connections returns connection info.
func TestE2E_ConnectionsEndpoint(t *testing.T) {
	conns := getJSON(t, apiBase+"/connections")

	active, ok := conns["active_connections"].(float64)
	if !ok {
		t.Fatal("missing active_connections")
	}
	t.Logf("active connections: %v", active)

	byBackend, ok := conns["by_backend"].(map[string]any)
	if !ok {
		t.Fatal("missing by_backend")
	}
	t.Logf("by_backend: %v", byBackend)
}

// Test 7: POST /reload succeeds.
func TestE2E_Reload(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(apiBase+"/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /reload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] == "" {
		t.Error("expected status in reload response")
	}
	t.Logf("reload response: %v", result)
}

// Test 8: Prometheus metrics endpoint is reachable.
func TestE2E_Metrics(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(metricsBase + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Verify our custom metrics are present.
	expectedMetrics := []string{
		"pgbastion_proxy_connections_total",
		"pgbastion_vip_transitions_total",
		"pgbastion_raft_state",
		"pgbastion_raft_is_leader",
		"pgbastion_raft_term",
		"pgbastion_api_request_duration_seconds",
		"pgbastion_api_requests_total",
	}
	for _, metric := range expectedMetrics {
		if !strings.Contains(bodyStr, metric) {
			t.Errorf("missing metric: %s", metric)
		}
	}
}

// Test 9: Compatibility endpoint /primary returns status.
func TestE2E_PrimaryEndpoint(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiBase + "/primary")
	if err != nil {
		t.Fatalf("GET /primary: %v", err)
	}
	defer resp.Body.Close()

	// Without cluster management, this may return 503.
	t.Logf("/primary status: %d", resp.StatusCode)
}

// Test 10: Compatibility endpoint /replica returns status.
func TestE2E_ReplicaEndpoint(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiBase + "/replica")
	if err != nil {
		t.Fatalf("GET /replica: %v", err)
	}
	defer resp.Body.Close()

	// Without cluster management, this may return 503.
	t.Logf("/replica status: %d", resp.StatusCode)
}

// Test 11: Compatibility endpoint /leader returns status.
func TestE2E_LeaderEndpoint(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiBase + "/leader")
	if err != nil {
		t.Fatalf("GET /leader: %v", err)
	}
	defer resp.Body.Close()

	t.Logf("/leader status: %d", resp.StatusCode)
}

const authToken = "e2e-test-token-12345"

// Test 12: Mutation endpoints require authentication.
func TestE2E_AuthRequired(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Test POST to /cluster/pause without auth - should return 401.
	resp, err := client.Post(apiBase+"/cluster/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /cluster/pause: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d", resp.StatusCode)
	}
	t.Logf("/cluster/pause without auth: %d", resp.StatusCode)
}

// Test 13: Mutation endpoints work with valid token.
func TestE2E_AuthWithValidToken(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Test POST to /cluster/pause with valid auth token.
	req, _ := http.NewRequest(http.MethodPost, apiBase+"/cluster/pause", nil)
	req.Header.Set("Authorization", "Bearer "+authToken)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /cluster/pause: %v", err)
	}
	defer resp.Body.Close()

	// Should return 404 (cluster manager not configured) or 200, not 401/403.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Errorf("expected auth to succeed, got %d", resp.StatusCode)
	}
	t.Logf("/cluster/pause with valid token: %d", resp.StatusCode)
}

// Test 14: Invalid token returns 403.
func TestE2E_AuthWithInvalidToken(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	req, _ := http.NewRequest(http.MethodPost, apiBase+"/cluster/pause", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /cluster/pause: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for invalid token, got %d", resp.StatusCode)
	}
	t.Logf("/cluster/pause with invalid token: %d", resp.StatusCode)
}

// Test 15: Read endpoints don't require auth.
func TestE2E_ReadEndpointsNoAuth(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// These should all work without auth.
	endpoints := []string{"/health", "/cluster", "/raft/state", "/raft/peers"}

	for _, ep := range endpoints {
		resp, err := client.Get(apiBase + ep)
		if err != nil {
			t.Errorf("GET %s: %v", ep, err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Errorf("%s should not require auth, got %d", ep, resp.StatusCode)
		}
	}
}
