package config

import (
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		Node: NodeConfig{
			Name:    "node1",
			Address: "10.0.1.1",
		},
		Raft: RaftConfig{
			DataDir:  "/var/lib/pgbastion/raft",
			BindPort: 8300,
		},
		Memberlist: MemberlistConfig{
			BindPort: 7946,
		},
		Proxy: ProxyConfig{
			PrimaryPort:  5432,
			ReplicaPort:  5433,
			DrainTimeout: 30 * time.Second,
		},
		API: APIConfig{Port: 8080},
		Log: LogConfig{Level: "info"},
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_EmptyNodeName(t *testing.T) {
	cfg := validConfig()
	cfg.Node.Name = ""
	assertValidationError(t, cfg, "node.name is required")
}

func TestValidate_EmptyNodeAddress(t *testing.T) {
	cfg := validConfig()
	cfg.Node.Address = ""
	assertValidationError(t, cfg, "node.address is required")
}

func TestValidate_NoRaftDataDir(t *testing.T) {
	cfg := validConfig()
	cfg.Raft.DataDir = ""
	assertValidationError(t, cfg, "raft.data_dir is required")
}

func TestValidate_PrimaryPortZero(t *testing.T) {
	cfg := validConfig()
	cfg.Proxy.PrimaryPort = 0
	assertValidationError(t, cfg, "proxy.primary_port must be between 1 and 65535")
}

func TestValidate_PrimaryPortTooHigh(t *testing.T) {
	cfg := validConfig()
	cfg.Proxy.PrimaryPort = 70000
	assertValidationError(t, cfg, "proxy.primary_port must be between 1 and 65535")
}

func TestValidate_ReplicaPortZero(t *testing.T) {
	cfg := validConfig()
	cfg.Proxy.ReplicaPort = 0
	assertValidationError(t, cfg, "proxy.replica_port must be between 1 and 65535")
}

func TestValidate_SameProxyPorts(t *testing.T) {
	cfg := validConfig()
	cfg.Proxy.ReplicaPort = cfg.Proxy.PrimaryPort
	assertValidationError(t, cfg, "proxy.primary_port and proxy.replica_port must be different")
}

func TestValidate_VIPAddressInvalid(t *testing.T) {
	cfg := validConfig()
	cfg.VIP.Address = "not-an-ip"
	cfg.VIP.Interface = "eth0"
	assertValidationError(t, cfg, "is not a valid IP address")
}

func TestValidate_VIPWithoutInterface(t *testing.T) {
	cfg := validConfig()
	cfg.VIP.Address = "10.0.1.100"
	cfg.VIP.Interface = ""
	assertValidationError(t, cfg, "vip.interface is required when vip.address is set")
}

func TestValidate_VIPFullyConfigured(t *testing.T) {
	cfg := validConfig()
	cfg.VIP.Address = "10.0.1.100"
	cfg.VIP.Interface = "eth0"
	if err := Validate(cfg); err != nil {
		t.Fatalf("fully configured VIP should be valid, got: %v", err)
	}
}

func TestValidate_NoVIP_StillValid(t *testing.T) {
	cfg := validConfig()
	cfg.VIP.Address = ""
	if err := Validate(cfg); err != nil {
		t.Fatalf("no VIP should be valid, got: %v", err)
	}
}

func TestValidate_APIPortInvalid(t *testing.T) {
	cfg := validConfig()
	cfg.API.Port = 0
	assertValidationError(t, cfg, "api.port must be between 1 and 65535")
}

func TestValidate_APIPortMax(t *testing.T) {
	cfg := validConfig()
	cfg.API.Port = 65535
	if err := Validate(cfg); err != nil {
		t.Fatalf("port 65535 should be valid, got: %v", err)
	}
}

func TestValidate_LogLevelInvalid(t *testing.T) {
	cfg := validConfig()
	cfg.Log.Level = "trace"
	assertValidationError(t, cfg, "is invalid; must be debug, info, warn, or error")
}

func TestValidate_LogLevelAllValid(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg := validConfig()
		cfg.Log.Level = level
		if err := Validate(cfg); err != nil {
			t.Errorf("log level %q should be valid, got: %v", level, err)
		}
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := &Config{
		Proxy: ProxyConfig{PrimaryPort: 5432, ReplicaPort: 5432},
		API:   APIConfig{Port: 8080},
		Log:   LogConfig{Level: "info"},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected multiple validation errors")
	}
	msg := err.Error()
	if !strings.Contains(msg, "node.name") {
		t.Error("expected node.name error")
	}
	if !strings.Contains(msg, "node.address") {
		t.Error("expected node.address error")
	}
	if !strings.Contains(msg, "raft.data_dir") {
		t.Error("expected raft.data_dir error")
	}
	if !strings.Contains(msg, "must be different") {
		t.Error("expected same-port error")
	}
}

func assertValidationError(t *testing.T, cfg *Config, substr string) {
	t.Helper()
	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected validation error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("expected error containing %q, got: %v", substr, err)
	}
}

func TestValidate_ClusterManagement_Valid(t *testing.T) {
	cfg := validConfig()
	cfg.PostgreSQL.DataDir = "/var/lib/postgresql/16/main"
	cfg.PostgreSQL.BinDir = "/usr/lib/postgresql/16/bin"
	cfg.PostgreSQL.ConnectDSN = "postgres://localhost:5432/postgres"
	cfg.PostgreSQL.Port = 5432
	cfg.Failover.ConfirmationPeriod = 5 * time.Second

	if err := Validate(cfg); err != nil {
		t.Fatalf("cluster management config should be valid, got: %v", err)
	}
}

func TestValidate_ClusterManagement_MissingBinDir(t *testing.T) {
	cfg := validConfig()
	cfg.PostgreSQL.DataDir = "/var/lib/postgresql/16/main"
	cfg.PostgreSQL.ConnectDSN = "postgres://localhost:5432/postgres"
	cfg.PostgreSQL.Port = 5432
	cfg.Failover.ConfirmationPeriod = 5 * time.Second

	assertValidationError(t, cfg, "postgresql.bin_dir is required")
}

func TestValidate_ClusterManagement_MissingConnectDSN(t *testing.T) {
	cfg := validConfig()
	cfg.PostgreSQL.DataDir = "/var/lib/postgresql/16/main"
	cfg.PostgreSQL.BinDir = "/usr/lib/postgresql/16/bin"
	cfg.PostgreSQL.Port = 5432
	cfg.Failover.ConfirmationPeriod = 5 * time.Second

	assertValidationError(t, cfg, "postgresql.connect_dsn is required")
}
