package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/spf13/cobra"

	"github.com/jrumley/pgbastion/internal/api"
	"github.com/jrumley/pgbastion/internal/cluster"
	"github.com/jrumley/pgbastion/internal/config"
	"github.com/jrumley/pgbastion/internal/consensus"
	"github.com/jrumley/pgbastion/internal/proxy"
	"github.com/jrumley/pgbastion/internal/tracing"
	"github.com/jrumley/pgbastion/internal/vip"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
	cfgFile   string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "pgbastion",
		Short: "Unified PostgreSQL high-availability daemon",
		Long:  "A single binary for PostgreSQL HA clusters with built-in consensus, proxy, and VIP management.",
	}

	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "/etc/pgbastion/pgbastion.yaml", "config file path")

	rootCmd.AddCommand(startCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(versionCmd())

	// Phase three commands (stubs for now).
	rootCmd.AddCommand(failoverCmd())
	rootCmd.AddCommand(switchoverCmd())
	rootCmd.AddCommand(reinitializeCmd())
	rootCmd.AddCommand(pauseCmd())
	rootCmd.AddCommand(resumeCmd())
	rootCmd.AddCommand(addNodeCmd())
	rootCmd.AddCommand(removeNodeCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the pgbastion daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemon()
		},
	}
}

func runDaemon() error {
	reloadableCfg, err := config.NewReloadableConfig(cfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return err
	}
	cfg := reloadableCfg.Get()

	logger := setupLogger(cfg.Log.Level)
	logger.Info("starting pgbastion",
		"version", version,
		"node", cfg.Node.Name,
		"address", cfg.Node.Address,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize distributed tracing.
	tracingCfg := tracing.Config{
		Enabled:     cfg.Tracing.Enabled,
		Exporter:    cfg.Tracing.Exporter,
		Endpoint:    cfg.Tracing.Endpoint,
		ServiceName: cfg.Tracing.ServiceName,
		SampleRate:  cfg.Tracing.SampleRate,
		Insecure:    cfg.Tracing.Insecure,
	}
	tracingShutdown, err := tracing.Init(ctx, tracingCfg, logger)
	if err != nil {
		logger.Error("failed to initialize tracing", "error", err)
		// Continue without tracing - it's not critical.
	} else {
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := tracingShutdown(shutdownCtx); err != nil {
				logger.Warn("failed to shutdown tracing", "error", err)
			}
		}()
	}

	// Handle OS signals for graceful shutdown and config reload.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for {
			sig := <-sigCh
			switch sig {
			case syscall.SIGHUP:
				logger.Info("received SIGHUP, reloading configuration")
				changes, err := reloadableCfg.Reload()
				if err != nil {
					logger.Error("failed to reload configuration", "error", err)
				} else if len(changes) > 0 {
					logger.Info("configuration reloaded", "changes", changes)
					// Update logger level if changed.
					newCfg := reloadableCfg.Get()
					if cfg.Log.Level != newCfg.Log.Level {
						logger = setupLogger(newCfg.Log.Level)
						logger.Info("log level changed", "level", newCfg.Log.Level)
					}
				} else {
					logger.Info("configuration reloaded, no changes detected")
				}
			case syscall.SIGTERM, syscall.SIGINT:
				logger.Info("received signal, initiating graceful shutdown", "signal", sig)
				cancel()
				return
			}
		}
	}()

	// Raft is required for pgbastion.
	if cfg.Raft.DataDir == "" {
		return fmt.Errorf("raft.data_dir is required")
	}

	// Initialize memberlist for peer discovery.
	if len(cfg.Cluster.Peers) > 0 {
		ml, err := initMemberlist(cfg, logger)
		if err != nil {
			logger.Error("failed to initialize memberlist", "error", err)
			return err
		}
		defer ml.Shutdown()
	}

	// Initialize FSM, Raft, and Store.
	logger.Info("initializing Raft consensus layer")
	fsm := consensus.NewClusterFSM()
	raftOpts := consensus.RaftOptions{
		LogLevel:         cfg.Raft.LogLevel,
		SnapshotRetain:   cfg.Raft.SnapshotRetain,
		TransportTimeout: cfg.Raft.TransportTimeout,
		HeartbeatTimeout: cfg.Raft.HeartbeatTimeout,
		ElectionTimeout:  cfg.Raft.ElectionTimeout,
		TLSEnabled:       cfg.Raft.TLSEnabled,
		TLSCert:          cfg.Raft.TLSCert,
		TLSKey:           cfg.Raft.TLSKey,
		TLSCA:            cfg.Raft.TLSCA,
	}
	raftInstance, raftTeardown, err := consensus.NewRaftNode(
		cfg.Node.Name,
		cfg.Node.Address,
		cfg.Raft.BindPort,
		cfg.Raft.DataDir,
		cfg.Raft.Bootstrap,
		raftOpts,
		fsm,
		logger,
	)
	if err != nil {
		return fmt.Errorf("initializing raft: %w", err)
	}
	defer raftTeardown()

	raftStore := consensus.NewStore(raftInstance, fsm)
	stateSource := consensus.NewRaftStateAdapter(fsm)

	// Start Raft metrics collector.
	raftMetrics := consensus.NewMetricsCollector(raftStore)
	raftMetrics.Start(5 * time.Second)
	defer raftMetrics.Stop()

	// Initialize proxy with Raft state source.
	router := proxy.NewRouter(stateSource, logger)

	// Initialize connection pool manager if pooling is enabled.
	var poolManager *proxy.PoolManager
	if cfg.Proxy.Pool.Enabled {
		poolCfg := proxy.PoolConfig{
			Enabled:           cfg.Proxy.Pool.Enabled,
			MaxPoolSize:       cfg.Proxy.Pool.MaxPoolSize,
			MinIdleConns:      cfg.Proxy.Pool.MinIdleConns,
			IdleTimeout:       cfg.Proxy.Pool.IdleTimeout,
			MaxConnLifetime:   cfg.Proxy.Pool.MaxConnLifetime,
			HealthCheckPeriod: cfg.Proxy.Pool.HealthCheckPeriod,
			AcquireTimeout:    cfg.Proxy.Pool.AcquireTimeout,
		}
		poolManager = proxy.NewPoolManager(poolCfg, cfg.Proxy.ConnectTimeout, logger)
		logger.Info("connection pooling enabled",
			"max_pool_size", poolCfg.MaxPoolSize,
			"min_idle_conns", poolCfg.MinIdleConns,
		)
	}

	proxyInstance := proxy.NewProxy(
		cfg.Proxy.PrimaryPort,
		cfg.Proxy.ReplicaPort,
		cfg.Proxy.DrainTimeout,
		cfg.Proxy.ConnectTimeout,
		router,
		poolManager,
		logger,
	)

	// Initialize VIP manager with Raft leadership.
	vipManager := vip.NewManager(
		cfg.VIP.Address,
		cfg.VIP.Interface,
		cfg.Node.Name,
		cfg.VIP.RenewInterval,
		vip.NewRaftLeadership(raftStore.RaftInstance()),
		logger,
	)

	// Cluster manager (optional - enabled when postgresql.data_dir is set).
	var clusterManager *cluster.Manager
	var healthChecker *cluster.HealthChecker
	var replicationMgr *cluster.ReplicationManager
	var progressTracker *cluster.ProgressTracker
	var configApplier *cluster.ConfigApplier
	var configWatcher *cluster.ConfigWatcher

	if cfg.IsClusterManagementEnabled() {
		logger.Info("initializing PostgreSQL cluster manager")

		healthChecker = cluster.NewHealthChecker(
			cfg.PostgreSQL.ConnectDSN,
			cfg.PostgreSQL.HealthInterval,
			cfg.PostgreSQL.HealthMaxBackoff,
			cfg.PostgreSQL.HealthBackoffMultiplier,
			cfg.PostgreSQL.HealthTimeout,
			logger,
		)
		failoverCtrl := cluster.NewFailoverController(cfg, raftStore, logger)
		replicationMgr = cluster.NewReplicationManager(cfg, raftStore, logger)
		progressTracker = cluster.NewProgressTracker(logger)
		clusterManager = cluster.NewManager(cfg, raftStore, healthChecker, failoverCtrl, replicationMgr, logger)

		// Initialize PostgreSQL config applier for cluster-wide config management.
		configApplier = cluster.NewConfigApplier(cluster.NewPgxConnector(), raftStore, logger)
		configWatcher = cluster.NewConfigWatcher(
			configApplier,
			cfg.PostgreSQL.ConnectDSN,
			10*time.Second, // Check for config changes every 10 seconds
			logger,
		)
	}

	// Initialize API server.
	apiServer := api.NewServer(
		cfg.API.Port,
		cfg.Metrics.Port,
		cfg.API.BindAddress,
		cfg.API.AuthToken,
		cfg.API.ShutdownTimeout,
		cfg.API.TLSCert,
		cfg.API.TLSKey,
		cfg.Node.Name,
		stateSource,
		proxyInstance,
		vipManager,
		raftStore,
		clusterManager,
		healthChecker,
		replicationMgr,
		progressTracker,
		configApplier,
		logger,
	)

	// Start all components.
	componentCount := 3
	if clusterManager != nil {
		componentCount += 2 // clusterManager + configWatcher
	}
	errCh := make(chan error, componentCount)

	go func() { errCh <- proxyInstance.Run(ctx) }()
	go func() { errCh <- vipManager.Run(ctx) }()
	go func() { errCh <- apiServer.Run(ctx) }()
	if clusterManager != nil {
		go func() { errCh <- clusterManager.Run(ctx) }()
		go func() { errCh <- configWatcher.Run(ctx) }()
	}

	// Wait for first error or shutdown.
	select {
	case err := <-errCh:
		if ctx.Err() != nil {
			logger.Info("pgbastion shut down gracefully")
			return nil
		}
		logger.Error("component failed", "error", err)
		cancel()
		return err
	case <-ctx.Done():
		logger.Info("pgbastion shut down gracefully")
		return nil
	}
}

func initMemberlist(cfg *config.Config, logger *slog.Logger) (*memberlist.Memberlist, error) {
	mlCfg := memberlist.DefaultLANConfig()
	mlCfg.Name = cfg.Node.Name
	mlCfg.BindAddr = cfg.Node.Address
	mlCfg.BindPort = cfg.Memberlist.BindPort
	mlCfg.AdvertiseAddr = cfg.Node.Address
	mlCfg.AdvertisePort = cfg.Memberlist.BindPort
	mlCfg.LogOutput = os.Stderr

	ml, err := memberlist.Create(mlCfg)
	if err != nil {
		return nil, fmt.Errorf("creating memberlist: %w", err)
	}

	if len(cfg.Cluster.Peers) > 0 {
		n, err := ml.Join(cfg.Cluster.Peers)
		if err != nil {
			logger.Warn("failed to join some memberlist peers", "error", err, "joined", n)
		} else {
			logger.Info("joined memberlist peers", "count", n)
		}
	}

	return ml, nil
}

func setupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lvl,
	}))
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cluster status",
		RunE: func(cmd *cobra.Command, args []string) error {
			apiPort := 8080

			// If a config file exists, try to read the API port from it.
			if cfg, err := config.Load(cfgFile); err == nil {
				apiPort = cfg.API.Port
			}

			baseURL := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
			client := &http.Client{Timeout: 5 * time.Second}

			// Fetch /health
			healthResp, err := client.Get(baseURL + "/health")
			if err != nil {
				return fmt.Errorf("pgbastion is not running or not reachable at %s: %w", baseURL, err)
			}
			defer healthResp.Body.Close()

			var health struct {
				Status     string `json:"status"`
				Node       string `json:"node"`
				Role       string `json:"role"`
				SplitBrain bool   `json:"split_brain"`
				VIPOwner   bool   `json:"vip_owner"`
			}
			if err := json.NewDecoder(healthResp.Body).Decode(&health); err != nil {
				return fmt.Errorf("decoding /health response: %w", err)
			}

			// Fetch /cluster
			clusterResp, err := client.Get(baseURL + "/cluster")
			if err != nil {
				return fmt.Errorf("fetching /cluster: %w", err)
			}
			defer clusterResp.Body.Close()

			var cluster struct {
				RoutingTable struct {
					Primary    string   `json:"primary"`
					Replicas   []string `json:"replicas"`
					SplitBrain bool     `json:"split_brain"`
				} `json:"routing_table"`
			}
			if err := json.NewDecoder(clusterResp.Body).Decode(&cluster); err != nil {
				return fmt.Errorf("decoding /cluster response: %w", err)
			}

			// Fetch /connections
			connResp, err := client.Get(baseURL + "/connections")
			if err != nil {
				return fmt.Errorf("fetching /connections: %w", err)
			}
			defer connResp.Body.Close()

			var connections struct {
				ActiveConnections int            `json:"active_connections"`
				ByBackend         map[string]int `json:"by_backend"`
			}
			if err := json.NewDecoder(connResp.Body).Decode(&connections); err != nil {
				return fmt.Errorf("decoding /connections response: %w", err)
			}

			// Format output.
			fmt.Println("=== pgbastion status ===")
			fmt.Printf("Node:        %s\n", health.Node)
			fmt.Printf("Status:      %s\n", health.Status)
			fmt.Printf("Role:        %s\n", health.Role)
			fmt.Printf("VIP Owner:   %v\n", health.VIPOwner)

			if health.SplitBrain {
				fmt.Println("SPLIT-BRAIN: *** DETECTED ***")
			}

			fmt.Println()
			fmt.Println("=== routing table ===")
			if cluster.RoutingTable.Primary != "" {
				fmt.Printf("Primary:     %s\n", cluster.RoutingTable.Primary)
			} else {
				fmt.Println("Primary:     (none)")
			}
			if len(cluster.RoutingTable.Replicas) > 0 {
				for i, r := range cluster.RoutingTable.Replicas {
					if i == 0 {
						fmt.Printf("Replicas:    %s\n", r)
					} else {
						fmt.Printf("             %s\n", r)
					}
				}
			} else {
				fmt.Println("Replicas:    (none)")
			}

			fmt.Println()
			fmt.Printf("=== connections: %d active ===\n", connections.ActiveConnections)
			if len(connections.ByBackend) > 0 {
				for backend, count := range connections.ByBackend {
					fmt.Printf("  %s: %d\n", backend, count)
				}
			}

			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("pgbastion %s (commit: %s, built: %s)\n", version, commit, buildDate)
		},
	}
}

// Phase three CLI commands — call the local REST API.

func cliAPIPort() int {
	if cfg, err := config.Load(cfgFile); err == nil {
		return cfg.API.Port
	}
	return 8080
}

func cliPost(endpoint string, body any) error {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", cliAPIPort())
	client := &http.Client{Timeout: 30 * time.Second}

	var reqBody []byte
	var err error
	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
	}

	resp, err := client.Post(baseURL+endpoint, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("pgbastion is not running or not reachable: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if resp.StatusCode >= 400 {
		if errMsg, ok := result["error"]; ok {
			return fmt.Errorf("%v", errMsg)
		}
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	if status, ok := result["status"]; ok {
		fmt.Println(status)
	} else {
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
	}
	return nil
}

func failoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "failover",
		Short: "Trigger a manual failover",
		RunE: func(cmd *cobra.Command, args []string) error {
			to, _ := cmd.Flags().GetString("to")
			if to == "" {
				return fmt.Errorf("--to flag is required")
			}
			return cliPost("/cluster/failover", map[string]string{"to": to})
		},
	}
	cmd.Flags().String("to", "", "target node for failover")
	return cmd
}

func switchoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "switchover",
		Short: "Trigger a graceful switchover",
		RunE: func(cmd *cobra.Command, args []string) error {
			to, _ := cmd.Flags().GetString("to")
			if to == "" {
				return fmt.Errorf("--to flag is required")
			}
			return cliPost("/cluster/switchover", map[string]string{"to": to})
		},
	}
	cmd.Flags().String("to", "", "target node for switchover")
	return cmd
}

func reinitializeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reinitialize",
		Short: "Reinitialize a replica from the primary",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cliPost("/cluster/reinitialize", nil)
		},
	}
	cmd.Flags().String("node", "", "node to reinitialize (currently reinitializes local node)")
	return cmd
}

func pauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Pause automatic failover",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cliPost("/cluster/pause", nil)
		},
	}
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume automatic failover",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cliPost("/cluster/resume", nil)
		},
	}
}

func addNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-node",
		Short: "Add a node to the Raft cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			address, _ := cmd.Flags().GetString("address")
			name, _ := cmd.Flags().GetString("name")
			if address == "" || name == "" {
				return fmt.Errorf("--name and --address flags are required")
			}
			return cliPost("/raft/add-peer", map[string]string{"id": name, "address": address})
		},
	}
	cmd.Flags().String("address", "", "address of the new node (host:port)")
	cmd.Flags().String("name", "", "name/ID of the new node")
	return cmd
}

func removeNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-node",
		Short: "Remove a node from the Raft cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return fmt.Errorf("--name flag is required")
			}
			return cliPost("/raft/remove-peer", map[string]string{"id": name})
		},
	}
	cmd.Flags().String("name", "", "name of the node to remove")
	return cmd
}
