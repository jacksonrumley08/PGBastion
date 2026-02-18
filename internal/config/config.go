package config

import (
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ReloadableConfig wraps a Config with hot-reload capability.
type ReloadableConfig struct {
	mu       sync.RWMutex
	cfg      *Config
	path     string
	onChange []func(*Config)
}

// NewReloadableConfig creates a new ReloadableConfig from the given path.
func NewReloadableConfig(path string) (*ReloadableConfig, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	return &ReloadableConfig{
		cfg:  cfg,
		path: path,
	}, nil
}

// Get returns the current configuration.
func (rc *ReloadableConfig) Get() *Config {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.cfg
}

// OnChange registers a callback to be called when configuration is reloaded.
func (rc *ReloadableConfig) OnChange(fn func(*Config)) {
	rc.mu.Lock()
	rc.onChange = append(rc.onChange, fn)
	rc.mu.Unlock()
}

// Reload reloads the configuration from disk and notifies listeners.
// Returns a list of fields that changed, or an error if reload failed.
func (rc *ReloadableConfig) Reload() ([]string, error) {
	newCfg, err := Load(rc.path)
	if err != nil {
		return nil, fmt.Errorf("reloading config: %w", err)
	}

	rc.mu.Lock()
	oldCfg := rc.cfg
	changes := detectChanges(oldCfg, newCfg)
	rc.cfg = newCfg
	listeners := make([]func(*Config), len(rc.onChange))
	copy(listeners, rc.onChange)
	rc.mu.Unlock()

	// Notify listeners.
	for _, fn := range listeners {
		fn(newCfg)
	}

	return changes, nil
}

// detectChanges compares two configs and returns a list of changed fields.
// Only checks fields that can be hot-reloaded safely.
func detectChanges(old, new *Config) []string {
	var changes []string

	if old.Log.Level != new.Log.Level {
		changes = append(changes, "log.level")
	}
	if old.Failover.ConfirmationPeriod != new.Failover.ConfirmationPeriod {
		changes = append(changes, "failover.confirmation_period")
	}
	if old.Failover.MaxLagThreshold != new.Failover.MaxLagThreshold {
		changes = append(changes, "failover.max_lag_threshold")
	}
	if old.Failover.IsFencingEnabled() != new.Failover.IsFencingEnabled() {
		changes = append(changes, "failover.fencing_enabled")
	}
	if old.PostgreSQL.HealthTimeout != new.PostgreSQL.HealthTimeout {
		changes = append(changes, "postgresql.health_timeout")
	}
	if old.API.AuthToken != new.API.AuthToken {
		changes = append(changes, "api.auth_token")
	}

	return changes
}

// CurrentConfigVersion is the current configuration schema version.
// Increment this when making breaking changes to the config format.
const CurrentConfigVersion = 1

// Config represents the complete pgbastion configuration.
type Config struct {
	Version    int              `yaml:"version"`
	Node       NodeConfig       `yaml:"node"`
	Cluster    ClusterConfig    `yaml:"cluster"`
	Proxy      ProxyConfig      `yaml:"proxy"`
	VIP        VIPConfig        `yaml:"vip"`
	Raft       RaftConfig       `yaml:"raft"`
	Memberlist MemberlistConfig `yaml:"memberlist"`
	PostgreSQL PostgreSQLConfig `yaml:"postgresql"`
	Failover   FailoverConfig   `yaml:"failover"`
	API        APIConfig        `yaml:"api"`
	Log        LogConfig        `yaml:"log"`
	Metrics    MetricsConfig    `yaml:"metrics"`
	Tracing    TracingConfig    `yaml:"tracing"`
}

// IsClusterManagementEnabled returns true when PostgreSQL cluster management is configured.
func (c *Config) IsClusterManagementEnabled() bool {
	return c.PostgreSQL.DataDir != ""
}

type NodeConfig struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
}

type ClusterConfig struct {
	Peers []string `yaml:"peers"`
}

type ProxyConfig struct {
	PrimaryPort    int           `yaml:"primary_port"`
	ReplicaPort    int           `yaml:"replica_port"`
	DrainTimeout   time.Duration `yaml:"drain_timeout"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	Pool           PoolConfig    `yaml:"pool"`
}

// PoolConfig holds connection pool configuration for the proxy.
type PoolConfig struct {
	Enabled           bool          `yaml:"enabled"`
	MaxPoolSize       int           `yaml:"max_pool_size"`       // per backend
	MinIdleConns      int           `yaml:"min_idle_conns"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxConnLifetime   time.Duration `yaml:"max_conn_lifetime"`
	HealthCheckPeriod time.Duration `yaml:"health_check_period"`
	AcquireTimeout    time.Duration `yaml:"acquire_timeout"`
}

type VIPConfig struct {
	Address       string        `yaml:"address"`
	Interface     string        `yaml:"interface"`
	RenewInterval time.Duration `yaml:"renew_interval"`
}

type RaftConfig struct {
	DataDir          string        `yaml:"data_dir"`
	BindPort         int           `yaml:"bind_port"`
	Bootstrap        bool          `yaml:"bootstrap"`
	LogLevel         string        `yaml:"log_level"`
	SnapshotRetain   int           `yaml:"snapshot_retain"`
	TransportTimeout time.Duration `yaml:"transport_timeout"`
	HeartbeatTimeout time.Duration `yaml:"heartbeat_timeout"`
	ElectionTimeout  time.Duration `yaml:"election_timeout"`

	// TLS configuration for encrypted Raft transport.
	TLSEnabled bool   `yaml:"tls_enabled"`
	TLSCert    string `yaml:"tls_cert"` // Path to TLS certificate
	TLSKey     string `yaml:"tls_key"`  // Path to TLS private key
	TLSCA      string `yaml:"tls_ca"`   // Path to CA certificate (enables mTLS)
}

type MemberlistConfig struct {
	BindPort int `yaml:"bind_port"`
}

type APIConfig struct {
	Port            int           `yaml:"port"`
	BindAddress     string        `yaml:"bind_address"`
	AuthToken       string        `yaml:"auth_token"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	TLSCert         string        `yaml:"tls_cert"`
	TLSKey          string        `yaml:"tls_key"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type MetricsConfig struct {
	Port int `yaml:"port"`
}

// PostgreSQLConfig holds PostgreSQL-specific settings for phase three.
type PostgreSQLConfig struct {
	BinDir                  string        `yaml:"bin_dir"`
	DataDir                 string        `yaml:"data_dir"`
	Port                    int           `yaml:"port"`
	Superuser               string        `yaml:"superuser"`
	ReplicationUser         string        `yaml:"replication_user"`
	ConnectDSN              string        `yaml:"connect_dsn"`
	HealthTimeout           time.Duration `yaml:"health_timeout"`
	HealthInterval          time.Duration `yaml:"health_interval"`
	HealthMaxBackoff        time.Duration `yaml:"health_max_backoff"`
	HealthBackoffMultiplier float64       `yaml:"health_backoff_multiplier"`
}

// FailoverConfig holds failover behavior settings for phase three.
type FailoverConfig struct {
	ConfirmationPeriod   time.Duration `yaml:"confirmation_period"`
	MaxLagThreshold      int64         `yaml:"max_lag_threshold"`
	FencingEnabled       *bool         `yaml:"fencing_enabled"`        // nil = default (true), explicit false disables
	QuorumLossTimeout    time.Duration `yaml:"quorum_loss_timeout"`    // Time without quorum before self-fencing (default 30s)
	SplitBrainProtection *bool         `yaml:"split_brain_protection"` // nil = default (true)
}

// IsFencingEnabled returns whether fencing is enabled. Defaults to true for safety.
func (f FailoverConfig) IsFencingEnabled() bool {
	if f.FencingEnabled == nil {
		return true // Safe default: fencing enabled
	}
	return *f.FencingEnabled
}

// IsSplitBrainProtectionEnabled returns whether split-brain protection is enabled.
// Defaults to true for safety - self-fence if quorum is lost.
func (f FailoverConfig) IsSplitBrainProtectionEnabled() bool {
	if f.SplitBrainProtection == nil {
		return true // Safe default: protection enabled
	}
	return *f.SplitBrainProtection
}

// TracingConfig holds distributed tracing configuration.
type TracingConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Exporter    string  `yaml:"exporter"`     // "otlp", "stdout", "none"
	Endpoint    string  `yaml:"endpoint"`     // e.g., "localhost:4317" for OTLP
	ServiceName string  `yaml:"service_name"`
	SampleRate  float64 `yaml:"sample_rate"`  // 0.0-1.0
	Insecure    bool    `yaml:"insecure"`     // Use insecure connection for OTLP
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	setDefaults(cfg)

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

func setDefaults(cfg *Config) {
	if cfg.Proxy.PrimaryPort == 0 {
		cfg.Proxy.PrimaryPort = 5432
	}
	if cfg.Proxy.ReplicaPort == 0 {
		cfg.Proxy.ReplicaPort = 5433
	}
	if cfg.Proxy.DrainTimeout == 0 {
		cfg.Proxy.DrainTimeout = 30 * time.Second
	}
	if cfg.Proxy.ConnectTimeout == 0 {
		cfg.Proxy.ConnectTimeout = 5 * time.Second
	}
	// Pool defaults.
	if cfg.Proxy.Pool.MaxPoolSize == 0 {
		cfg.Proxy.Pool.MaxPoolSize = 20
	}
	if cfg.Proxy.Pool.MinIdleConns == 0 {
		cfg.Proxy.Pool.MinIdleConns = 2
	}
	if cfg.Proxy.Pool.IdleTimeout == 0 {
		cfg.Proxy.Pool.IdleTimeout = 5 * time.Minute
	}
	if cfg.Proxy.Pool.MaxConnLifetime == 0 {
		cfg.Proxy.Pool.MaxConnLifetime = 30 * time.Minute
	}
	if cfg.Proxy.Pool.HealthCheckPeriod == 0 {
		cfg.Proxy.Pool.HealthCheckPeriod = 30 * time.Second
	}
	if cfg.Proxy.Pool.AcquireTimeout == 0 {
		cfg.Proxy.Pool.AcquireTimeout = 5 * time.Second
	}
	if cfg.VIP.RenewInterval == 0 {
		cfg.VIP.RenewInterval = 3 * time.Second
	}
	if cfg.Raft.BindPort == 0 {
		cfg.Raft.BindPort = 8300
	}
	if cfg.Raft.LogLevel == "" {
		cfg.Raft.LogLevel = "WARN"
	}
	if cfg.Raft.SnapshotRetain == 0 {
		cfg.Raft.SnapshotRetain = 2
	}
	if cfg.Raft.TransportTimeout == 0 {
		cfg.Raft.TransportTimeout = 10 * time.Second
	}
	if cfg.Raft.HeartbeatTimeout == 0 {
		cfg.Raft.HeartbeatTimeout = 1 * time.Second
	}
	if cfg.Raft.ElectionTimeout == 0 {
		cfg.Raft.ElectionTimeout = 1 * time.Second
	}
	if cfg.Memberlist.BindPort == 0 {
		cfg.Memberlist.BindPort = 7946
	}
	if cfg.API.Port == 0 {
		cfg.API.Port = 8080
	}
	if cfg.API.BindAddress == "" {
		cfg.API.BindAddress = "127.0.0.1"
	}
	if cfg.API.ShutdownTimeout == 0 {
		cfg.API.ShutdownTimeout = 5 * time.Second
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Metrics.Port == 0 {
		cfg.Metrics.Port = 9090
	}
	if cfg.PostgreSQL.Port == 0 {
		cfg.PostgreSQL.Port = 5432
	}
	if cfg.PostgreSQL.Superuser == "" {
		cfg.PostgreSQL.Superuser = "postgres"
	}
	if cfg.PostgreSQL.ReplicationUser == "" {
		cfg.PostgreSQL.ReplicationUser = "replicator"
	}
	if cfg.Failover.ConfirmationPeriod == 0 {
		cfg.Failover.ConfirmationPeriod = 5 * time.Second
	}
	if cfg.Failover.QuorumLossTimeout == 0 {
		cfg.Failover.QuorumLossTimeout = 30 * time.Second
	}
	if cfg.PostgreSQL.HealthTimeout == 0 {
		cfg.PostgreSQL.HealthTimeout = 5 * time.Second
	}
	if cfg.PostgreSQL.HealthInterval == 0 {
		cfg.PostgreSQL.HealthInterval = 2 * time.Second
	}
	if cfg.PostgreSQL.HealthMaxBackoff == 0 {
		cfg.PostgreSQL.HealthMaxBackoff = 30 * time.Second
	}
	if cfg.PostgreSQL.HealthBackoffMultiplier == 0 {
		cfg.PostgreSQL.HealthBackoffMultiplier = 2.0
	}
	// Tracing defaults.
	if cfg.Tracing.Exporter == "" {
		cfg.Tracing.Exporter = "otlp"
	}
	if cfg.Tracing.Endpoint == "" {
		cfg.Tracing.Endpoint = "localhost:4317"
	}
	if cfg.Tracing.ServiceName == "" {
		cfg.Tracing.ServiceName = "pgbastion"
	}
	if cfg.Tracing.SampleRate == 0 && cfg.Tracing.Enabled {
		cfg.Tracing.SampleRate = 1.0
	}
}
