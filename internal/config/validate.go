package config

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"
)

// Validate checks the configuration for errors before any network connections are made.
func Validate(cfg *Config) error {
	var errs []string

	// Version validation.
	if cfg.Version == 0 {
		// Default to current version if not specified (backwards compatible).
		cfg.Version = CurrentConfigVersion
	} else if cfg.Version > CurrentConfigVersion {
		errs = append(errs, fmt.Sprintf("config version %d is newer than supported version %d; please upgrade pgbastion", cfg.Version, CurrentConfigVersion))
	} else if cfg.Version < CurrentConfigVersion {
		errs = append(errs, fmt.Sprintf("config version %d is deprecated; please update your config to version %d (see migration guide)", cfg.Version, CurrentConfigVersion))
	}

	if cfg.Node.Name == "" {
		errs = append(errs, "node.name is required")
	}
	if cfg.Node.Address == "" {
		errs = append(errs, "node.address is required")
	}

	// Raft is required for pgbastion.
	if cfg.Raft.DataDir == "" {
		errs = append(errs, "raft.data_dir is required")
	}

	if cfg.Proxy.PrimaryPort < 1 || cfg.Proxy.PrimaryPort > 65535 {
		errs = append(errs, "proxy.primary_port must be between 1 and 65535")
	}
	if cfg.Proxy.ReplicaPort < 1 || cfg.Proxy.ReplicaPort > 65535 {
		errs = append(errs, "proxy.replica_port must be between 1 and 65535")
	}
	if cfg.Proxy.PrimaryPort == cfg.Proxy.ReplicaPort {
		errs = append(errs, "proxy.primary_port and proxy.replica_port must be different")
	}

	if cfg.VIP.Address != "" {
		if ip := net.ParseIP(cfg.VIP.Address); ip == nil {
			errs = append(errs, fmt.Sprintf("vip.address %q is not a valid IP address", cfg.VIP.Address))
		}
		if cfg.VIP.Interface == "" {
			errs = append(errs, "vip.interface is required when vip.address is set")
		}
	}

	if cfg.Raft.DataDir != "" {
		if cfg.Raft.BindPort < 1 || cfg.Raft.BindPort > 65535 {
			errs = append(errs, "raft.bind_port must be between 1 and 65535")
		}
		if cfg.Memberlist.BindPort < 1 || cfg.Memberlist.BindPort > 65535 {
			errs = append(errs, "memberlist.bind_port must be between 1 and 65535")
		}
		// Check for port conflicts with other services.
		ports := map[int]string{
			cfg.Proxy.PrimaryPort: "proxy.primary_port",
			cfg.Proxy.ReplicaPort: "proxy.replica_port",
			cfg.API.Port:          "api.port",
			cfg.Metrics.Port:      "metrics.port",
		}
		if name, ok := ports[cfg.Raft.BindPort]; ok {
			errs = append(errs, fmt.Sprintf("raft.bind_port %d conflicts with %s", cfg.Raft.BindPort, name))
		}
		if name, ok := ports[cfg.Memberlist.BindPort]; ok {
			errs = append(errs, fmt.Sprintf("memberlist.bind_port %d conflicts with %s", cfg.Memberlist.BindPort, name))
		}
		if cfg.Raft.BindPort == cfg.Memberlist.BindPort {
			errs = append(errs, "raft.bind_port and memberlist.bind_port must be different")
		}
	}

	// PostgreSQL cluster management validation.
	if cfg.PostgreSQL.DataDir != "" {
		if cfg.PostgreSQL.BinDir == "" {
			errs = append(errs, "postgresql.bin_dir is required when postgresql.data_dir is set")
		} else {
			// Validate bin_dir is an absolute path and doesn't contain dangerous characters.
			if !filepath.IsAbs(cfg.PostgreSQL.BinDir) {
				errs = append(errs, "postgresql.bin_dir must be an absolute path")
			}
			if containsShellChars(cfg.PostgreSQL.BinDir) {
				errs = append(errs, "postgresql.bin_dir contains invalid characters")
			}
		}
		// Validate data_dir is an absolute path.
		if !filepath.IsAbs(cfg.PostgreSQL.DataDir) {
			errs = append(errs, "postgresql.data_dir must be an absolute path")
		}
		if containsShellChars(cfg.PostgreSQL.DataDir) {
			errs = append(errs, "postgresql.data_dir contains invalid characters")
		}
		if cfg.PostgreSQL.ConnectDSN == "" {
			errs = append(errs, "postgresql.connect_dsn is required when postgresql.data_dir is set")
		}
		if cfg.Raft.DataDir == "" {
			errs = append(errs, "postgresql.data_dir requires raft.data_dir")
		}
		if cfg.PostgreSQL.Port < 1 || cfg.PostgreSQL.Port > 65535 {
			errs = append(errs, "postgresql.port must be between 1 and 65535")
		}
		if cfg.Failover.ConfirmationPeriod < time.Second {
			errs = append(errs, "failover.confirmation_period must be at least 1s")
		}
	}

	if cfg.API.Port < 1 || cfg.API.Port > 65535 {
		errs = append(errs, "api.port must be between 1 and 65535")
	}
	if cfg.API.BindAddress != "" {
		if ip := net.ParseIP(cfg.API.BindAddress); ip == nil {
			errs = append(errs, fmt.Sprintf("api.bind_address %q is not a valid IP address", cfg.API.BindAddress))
		}
	}

	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("log.level %q is invalid; must be debug, info, warn, or error", cfg.Log.Level))
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return nil
}

// containsShellChars returns true if the path contains characters that could be
// used for shell injection attacks.
func containsShellChars(path string) bool {
	// Characters that could be dangerous in shell contexts.
	dangerous := []string{";", "&", "|", "$", "`", "(", ")", "{", "}", "<", ">", "!", "\n", "\r"}
	for _, char := range dangerous {
		if strings.Contains(path, char) {
			return true
		}
	}
	return false
}
