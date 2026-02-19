# pgbastion — Unified PostgreSQL High-Availability Daemon

A single Go binary that replaces the Patroni + etcd + HAProxy + Keepalived stack with one process, one config file, and one systemd service per node.

## Architecture

Each node runs an identical `pgbastion` binary handling four responsibilities:

| Component | Replaces | Implementation |
|---|---|---|
| TCP Proxy | HAProxy | Go `net` package, goroutine-per-connection |
| VIP Manager | Keepalived | `vishvananda/netlink`, gratuitous ARP (Linux only) |
| Consensus | etcd | `hashicorp/raft` embedded consensus |
| Cluster Manager | Patroni | `jackc/pgx`, `pg_ctl` |

## Features

- **TCP proxy** listens on port 5432 (primary) and 5433 (replicas), routing PostgreSQL connections based on Raft cluster state
- **Raft consensus** provides distributed state management and leader election
- **VIP management** assigns a virtual IP to the Raft leader with gratuitous ARP
- **PostgreSQL management** handles health checks, failover, switchover, and replication
- **Split-brain detection** refuses to route traffic when cluster state is inconsistent
- **Connection draining** gracefully drains connections during failover
- **REST API** for health checks, cluster state, and administrative operations
- **Prometheus metrics** for monitoring and alerting
- **Config hot reload** via SIGHUP signal

## Building

```bash
go build -ldflags "-X main.version=1.0.0" -o pgbastion ./cmd/pgbastion/
```

## Configuration

Copy `config/pgbastion.yaml.example` to `/etc/pgbastion/pgbastion.yaml` and edit for your environment.

### Required Fields

| Field | Description |
|---|---|
| `node.name` | Unique name for this node |
| `node.address` | This node's IP address |
| `raft.data_dir` | Directory for Raft state |

### Key Configuration

```yaml
node:
  name: "node1"
  address: "10.0.1.1"

cluster:
  peers:
    - "10.0.1.2"
    - "10.0.1.3"

raft:
  data_dir: "/var/lib/pgbastion/raft"
  bind_port: 8300
  bootstrap: true    # Only true on the first node

memberlist:
  bind_port: 7946

proxy:
  primary_port: 5432
  replica_port: 5433
  drain_timeout: 30s

vip:
  address: "10.0.1.100"
  interface: "eth0"

# Optional: PostgreSQL cluster management
postgresql:
  bin_dir: "/usr/lib/postgresql/16/bin"
  data_dir: "/var/lib/postgresql/16/main"
  connect_dsn: "postgres://localhost/postgres"

api:
  port: 8080

metrics:
  port: 9090

log:
  level: "info"
```

See `config/pgbastion.yaml.example` for complete reference.

## Deployment

1. Install the binary:
   ```bash
   sudo cp pgbastion /usr/local/bin/
   sudo chmod +x /usr/local/bin/pgbastion
   ```

2. Create the service user:
   ```bash
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin pgbastion
   sudo mkdir -p /var/lib/pgbastion /etc/pgbastion
   sudo chown pgbastion:pgbastion /var/lib/pgbastion
   ```

3. Deploy the config:
   ```bash
   sudo cp config/pgbastion.yaml.example /etc/pgbastion/pgbastion.yaml
   sudo vi /etc/pgbastion/pgbastion.yaml
   ```

4. Install and start the systemd service:
   ```bash
   sudo cp systemd/pgbastion.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now pgbastion
   ```

5. Verify:
   ```bash
   curl http://localhost:8080/health
   curl http://localhost:8080/cluster
   curl http://localhost:8080/raft/state
   ```

## Cluster Setup

### Bootstrap a New Cluster

On the first node:
```bash
# Set bootstrap: true in config
pgbastion start
```

On additional nodes:
```bash
# Set bootstrap: false in config
# List first node in cluster.peers
pgbastion start

# Add to Raft cluster from the leader
curl -X POST http://leader:8080/raft/add-peer \
  -d '{"id": "node2", "address": "10.0.1.2:8300"}'
```

### Failover and Switchover

```bash
# Manual failover (emergency)
pgbastion failover --to node2

# Graceful switchover
pgbastion switchover --to node2

# Pause automatic failover
pgbastion pause

# Resume automatic failover
pgbastion resume
```

## REST API

### Core Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/health` | GET | Node health, role, VIP status |
| `/cluster` | GET | Routing table with backends |
| `/connections` | GET | Active connection counts |
| `/reload` | POST | Trigger state refresh |

### Raft Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/raft/state` | GET | Raft role and leader info |
| `/raft/peers` | GET | All cluster peers |
| `/raft/add-peer` | POST | Add a new node |
| `/raft/remove-peer` | POST | Remove a node |
| `/raft/directives` | GET | Pending directives |

### Cluster Management

| Endpoint | Method | Description |
|---|---|---|
| `/cluster/state` | GET | Node state machine |
| `/cluster/replication` | GET | Replication status |
| `/cluster/failover` | POST | Manual failover |
| `/cluster/switchover` | POST | Graceful switchover |
| `/cluster/pause` | POST | Pause auto-failover |
| `/cluster/resume` | POST | Resume auto-failover |

### Compatibility Endpoints

For HAProxy-style health checks:

| Endpoint | Method | Description |
|---|---|---|
| `/primary` | GET | 200 if primary, 503 otherwise |
| `/replica` | GET | 200 if replica, 503 otherwise |
| `/read-write` | GET | 200 if accepting writes |
| `/read-only` | GET | 200 if accepting reads |

## CLI

```bash
pgbastion start              # Start the daemon
pgbastion status             # Show cluster status
pgbastion version            # Print version info
pgbastion failover --to X    # Manual failover
pgbastion switchover --to X  # Graceful switchover
pgbastion pause              # Pause auto-failover
pgbastion resume             # Resume auto-failover
pgbastion add-node --name X --address Y
pgbastion remove-node --name X
```

## Project Structure

```
pgbastion/
├── cmd/pgbastion/       # CLI entry point
├── internal/
│   ├── api/             # REST API server and handlers
│   ├── cluster/         # PostgreSQL cluster management
│   ├── config/          # YAML config loading and validation
│   ├── consensus/       # Raft consensus layer
│   ├── proxy/           # TCP proxy, router, connection tracking
│   └── vip/             # VIP assignment and gratuitous ARP
├── test/e2e/            # End-to-end tests
├── systemd/             # systemd service file
└── config/              # Example configuration
```

## Requirements

- Go 1.22+
- Linux, Windows, or macOS
  - **Linux:** Full functionality including VIP management via netlink
  - **Windows/macOS:** All features except VIP management (use load balancer instead)
- `CAP_NET_ADMIN` and `CAP_NET_RAW` capabilities (Linux only, for VIP)
- PostgreSQL 14+ (for cluster management features)

## Installation

See **[INSTALL.md](INSTALL.md)** for detailed step-by-step installation guides:

- [RHEL/CentOS/Rocky Linux](INSTALL.md#rhelcentosrockyalma-linux-8-or-9)
- [Debian/Ubuntu](INSTALL.md#debianubuntu)
- [Windows](INSTALL.md#windows-installation)

## Operations

See **[OPERATIONS.md](OPERATIONS.md)** for:

- Backup and restore procedures
- Rolling upgrades
- Monitoring and alerting
- Disaster recovery

## License

MIT
