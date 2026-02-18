package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_ValidConfig(t *testing.T) {
	yaml := `
node:
  name: "node1"
  address: "10.0.1.1"
raft:
  data_dir: "/var/lib/pgbastion/raft"
  bind_port: 8300
memberlist:
  bind_port: 7946
proxy:
  primary_port: 5432
  replica_port: 5433
api:
  port: 8080
log:
  level: "info"
`
	cfg := loadFromString(t, yaml)

	if cfg.Node.Name != "node1" {
		t.Errorf("expected node.name=node1, got %s", cfg.Node.Name)
	}
	if cfg.Node.Address != "10.0.1.1" {
		t.Errorf("expected node.address=10.0.1.1, got %s", cfg.Node.Address)
	}
}

func TestLoad_Defaults(t *testing.T) {
	yaml := `
node:
  name: "node1"
  address: "10.0.1.1"
raft:
  data_dir: "/var/lib/pgbastion/raft"
log:
  level: "info"
`
	cfg := loadFromString(t, yaml)

	if cfg.Proxy.PrimaryPort != 5432 {
		t.Errorf("expected default primary_port=5432, got %d", cfg.Proxy.PrimaryPort)
	}
	if cfg.Proxy.ReplicaPort != 5433 {
		t.Errorf("expected default replica_port=5433, got %d", cfg.Proxy.ReplicaPort)
	}
	if cfg.Proxy.DrainTimeout != 30*time.Second {
		t.Errorf("expected default drain_timeout=30s, got %s", cfg.Proxy.DrainTimeout)
	}
	if cfg.VIP.RenewInterval != 3*time.Second {
		t.Errorf("expected default renew_interval=3s, got %s", cfg.VIP.RenewInterval)
	}
	if cfg.API.Port != 8080 {
		t.Errorf("expected default api.port=8080, got %d", cfg.API.Port)
	}
	if cfg.Metrics.Port != 9090 {
		t.Errorf("expected default metrics.port=9090, got %d", cfg.Metrics.Port)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("expected default log.level=info, got %s", cfg.Log.Level)
	}
	if cfg.Raft.BindPort != 8300 {
		t.Errorf("expected default raft.bind_port=8300, got %d", cfg.Raft.BindPort)
	}
	if cfg.Memberlist.BindPort != 7946 {
		t.Errorf("expected default memberlist.bind_port=7946, got %d", cfg.Memberlist.BindPort)
	}
}

func TestLoad_OverrideDefaults(t *testing.T) {
	yaml := `
node:
  name: "node1"
  address: "10.0.1.1"
raft:
  data_dir: "/var/lib/pgbastion/raft"
  bind_port: 8400
memberlist:
  bind_port: 7900
proxy:
  primary_port: 6432
  replica_port: 6433
  drain_timeout: 60s
api:
  port: 9000
metrics:
  port: 9100
log:
  level: "debug"
`
	cfg := loadFromString(t, yaml)

	if cfg.Proxy.PrimaryPort != 6432 {
		t.Errorf("expected primary_port=6432, got %d", cfg.Proxy.PrimaryPort)
	}
	if cfg.Proxy.ReplicaPort != 6433 {
		t.Errorf("expected replica_port=6433, got %d", cfg.Proxy.ReplicaPort)
	}
	if cfg.Proxy.DrainTimeout != 60*time.Second {
		t.Errorf("expected drain_timeout=60s, got %s", cfg.Proxy.DrainTimeout)
	}
	if cfg.API.Port != 9000 {
		t.Errorf("expected api.port=9000, got %d", cfg.API.Port)
	}
	if cfg.Metrics.Port != 9100 {
		t.Errorf("expected metrics.port=9100, got %d", cfg.Metrics.Port)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("expected log.level=debug, got %s", cfg.Log.Level)
	}
	if cfg.Raft.BindPort != 8400 {
		t.Errorf("expected raft.bind_port=8400, got %d", cfg.Raft.BindPort)
	}
	if cfg.Memberlist.BindPort != 7900 {
		t.Errorf("expected memberlist.bind_port=7900, got %d", cfg.Memberlist.BindPort)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTempFile(t, "not: [valid: yaml: {{")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	path := writeTempFile(t, "")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for empty file")
	}
}

func TestLoad_VIPConfig(t *testing.T) {
	yaml := `
node:
  name: "node1"
  address: "10.0.1.1"
raft:
  data_dir: "/var/lib/pgbastion/raft"
vip:
  address: "10.0.1.100"
  interface: "eth0"
log:
  level: "info"
`
	cfg := loadFromString(t, yaml)

	if cfg.VIP.Address != "10.0.1.100" {
		t.Errorf("expected vip.address=10.0.1.100, got %s", cfg.VIP.Address)
	}
	if cfg.VIP.Interface != "eth0" {
		t.Errorf("expected vip.interface=eth0, got %s", cfg.VIP.Interface)
	}
}

func TestLoad_ClusterPeers(t *testing.T) {
	yaml := `
node:
  name: "node1"
  address: "10.0.1.1"
raft:
  data_dir: "/var/lib/pgbastion/raft"
cluster:
  peers:
    - "10.0.1.2"
    - "10.0.1.3"
log:
  level: "info"
`
	cfg := loadFromString(t, yaml)

	if len(cfg.Cluster.Peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(cfg.Cluster.Peers))
	}
	if cfg.Cluster.Peers[0] != "10.0.1.2" {
		t.Errorf("expected peer[0]=10.0.1.2, got %s", cfg.Cluster.Peers[0])
	}
}

func TestLoad_PostgreSQLConfig(t *testing.T) {
	yaml := `
node:
  name: "node1"
  address: "10.0.1.1"
raft:
  data_dir: "/var/lib/pgbastion/raft"
postgresql:
  bin_dir: "/usr/lib/postgresql/16/bin"
  data_dir: "/var/lib/postgresql/16/main"
  port: 5432
  connect_dsn: "postgres://localhost/postgres"
failover:
  confirmation_period: 5s
log:
  level: "info"
`
	cfg := loadFromString(t, yaml)

	if cfg.PostgreSQL.BinDir != "/usr/lib/postgresql/16/bin" {
		t.Errorf("expected postgresql.bin_dir, got %s", cfg.PostgreSQL.BinDir)
	}
	if cfg.PostgreSQL.DataDir != "/var/lib/postgresql/16/main" {
		t.Errorf("expected postgresql.data_dir, got %s", cfg.PostgreSQL.DataDir)
	}
	if cfg.PostgreSQL.Port != 5432 {
		t.Errorf("expected postgresql.port=5432, got %d", cfg.PostgreSQL.Port)
	}
}

// helpers

func loadFromString(t *testing.T, yamlContent string) *Config {
	t.Helper()
	path := writeTempFile(t, yamlContent)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}
	return cfg
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pgbastion.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}
