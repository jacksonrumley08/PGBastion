package cluster

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jrumley/pgbastion/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testConfig() *config.Config {
	fencingEnabled := true
	return &config.Config{
		Node: config.NodeConfig{Name: "node1", Address: "10.0.1.1"},
		PostgreSQL: config.PostgreSQLConfig{
			BinDir:     "/usr/lib/postgresql/16/bin",
			DataDir:    "/var/lib/postgresql/16/main",
			Port:       5432,
			ConnectDSN: "postgres://localhost:5432/postgres",
			Superuser:  "postgres",
		},
		Failover: config.FailoverConfig{
			ConfirmationPeriod: 5 * time.Second,
			FencingEnabled:     &fencingEnabled,
		},
	}
}

func TestManager_InitialState(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	if m.State() != StateInitializing {
		t.Errorf("expected INITIALIZING, got %s", m.State())
	}
}

func TestManager_PauseResume(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	if m.IsPaused() {
		t.Error("expected not paused initially")
	}

	m.Pause()
	if !m.IsPaused() {
		t.Error("expected paused after Pause()")
	}

	m.Resume()
	if m.IsPaused() {
		t.Error("expected not paused after Resume()")
	}
}

func TestManager_StateTransition_InitToReplica(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	// Simulate a healthy replica health status.
	hc.setStatus(HealthStatus{
		Healthy:      true,
		IsInRecovery: true,
	}, 0)

	m.evaluate(context.Background())

	if m.State() != StateRunningReplica {
		t.Errorf("expected RUNNING_REPLICA, got %s", m.State())
	}
}

func TestManager_StateTransition_InitToPrimary(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	// Simulate a healthy primary health status.
	hc.setStatus(HealthStatus{
		Healthy:      true,
		IsInRecovery: false,
	}, 0)

	m.evaluate(context.Background())

	if m.State() != StateRunningPrimary {
		t.Errorf("expected RUNNING_PRIMARY, got %s", m.State())
	}
}

func TestManager_StateTransition_PrimaryToRecovering(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	// Start as primary.
	m.setState(StateRunningPrimary)

	// Primary becomes unhealthy.
	hc.setStatus(HealthStatus{Healthy: false}, 0)

	m.evaluate(context.Background())

	if m.State() != StateRecovering {
		t.Errorf("expected RECOVERING, got %s", m.State())
	}
}

func TestManager_StateTransition_RecoveringToReplica(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	m.setState(StateRecovering)

	hc.setStatus(HealthStatus{Healthy: true, IsInRecovery: true}, 0)

	m.evaluate(context.Background())

	if m.State() != StateRunningReplica {
		t.Errorf("expected RUNNING_REPLICA, got %s", m.State())
	}
}

func TestManager_StateTransition_ReplicaLagging(t *testing.T) {
	cfg := testConfig()
	cfg.Failover.MaxLagThreshold = 1000
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	m.setState(StateRunningReplica)

	hc.setStatus(HealthStatus{
		Healthy:        true,
		IsInRecovery:   true,
		ReplicationLag: 5000,
	}, 0)

	m.evaluate(context.Background())

	if m.State() != StateReplicaLagging {
		t.Errorf("expected REPLICA_LAGGING, got %s", m.State())
	}
}

func TestManager_StateTransition_LaggingToReplica(t *testing.T) {
	cfg := testConfig()
	cfg.Failover.MaxLagThreshold = 1000
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	m.setState(StateReplicaLagging)

	hc.setStatus(HealthStatus{
		Healthy:        true,
		IsInRecovery:   true,
		ReplicationLag: 500,
	}, 0)

	m.evaluate(context.Background())

	if m.State() != StateRunningReplica {
		t.Errorf("expected RUNNING_REPLICA, got %s", m.State())
	}
}

func TestManager_FencedStaysFenced(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	m.setState(StateFenced)

	hc.setStatus(HealthStatus{Healthy: true}, 0)

	m.evaluate(context.Background())

	if m.State() != StateFenced {
		t.Errorf("expected FENCED to remain, got %s", m.State())
	}
}

func TestManager_ProcessDirectives_NoStore(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	m := NewManager(cfg, nil, hc, nil, nil, testLogger())

	// Should not panic when store is nil.
	m.processDirectives(context.Background())
}

func TestManager_WithExecutor(t *testing.T) {
	cfg := testConfig()
	hc := NewHealthCheckerWithConnector(cfg.PostgreSQL.ConnectDSN, 2*time.Second, &mockConnector{}, testLogger())
	exec := &mockExecutor{}
	m := NewManagerWithExecutor(cfg, nil, hc, nil, nil, exec, testLogger())

	if m.executor != exec {
		t.Error("expected custom executor to be set")
	}
}
