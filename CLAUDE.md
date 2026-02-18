# PGBastion — Project Reference

Standalone PostgreSQL HA daemon replacing Patroni + etcd + HAProxy + Keepalived.

---

## Overview

| Component | Replaces | Package |
|-----------|----------|---------|
| TCP Proxy | HAProxy | `internal/proxy/` |
| VIP Management | Keepalived | `internal/vip/` |
| Consensus | etcd | `internal/consensus/` |
| Cluster Manager | Patroni | `internal/cluster/` |

---

## Repository Structure

```
cmd/pgbastion/main.go     # CLI + daemon orchestration
internal/
  api/                    # REST API + Prometheus /metrics
  cluster/                # Health checks, failover, replication
  config/                 # YAML loading, validation, hot-reload
  consensus/              # Raft FSM, store, directives
  proxy/                  # TCP proxy, routing, connection tracking
  vip/                    # VIP management via netlink
test/e2e/                 # Docker-based E2E tests (15 tests)
```

---

## Configuration

Default: `/etc/pgbastion/pgbastion.yaml`

```yaml
node:
  name: node1
  address: 10.0.1.1

raft:
  data_dir: /var/lib/pgbastion/raft
  bind_port: 8300
  bootstrap: false  # true only for first node
  log_level: WARN   # DEBUG, INFO, WARN, ERROR
  snapshot_retain: 2
  transport_timeout: 10s
  heartbeat_timeout: 1s
  election_timeout: 1s

proxy:
  primary_port: 5432
  replica_port: 5433
  drain_timeout: 30s

vip:
  address: 10.0.1.100
  interface: eth0

postgresql:  # enables cluster management
  bin_dir: /usr/lib/postgresql/16/bin
  data_dir: /var/lib/postgresql/16/main
  connect_dsn: "host=/var/run/postgresql dbname=postgres"
  health_interval: 2s
  health_max_backoff: 30s
  health_backoff_multiplier: 2.0

api:
  port: 8080
  bind_address: 127.0.0.1  # default, use 0.0.0.0 for all interfaces
  auth_token: "your-secret-token"  # required for mutation endpoints

metrics:
  port: 9090

log:
  level: info
```

---

## Network Ports

| Port | Purpose |
|------|---------|
| 5432 | Primary proxy (writes) |
| 5433 | Replica proxy (reads) |
| 8080 | REST API |
| 9090 | Prometheus metrics |
| 8300 | Raft consensus |
| 7946 | Memberlist discovery |

---

## Key REST API Endpoints

| Endpoint | Method | Auth Required | Description |
|----------|--------|---------------|-------------|
| `/health` | GET | No | Node health status |
| `/cluster` | GET | No | Routing table |
| `/primary` | GET | No | 200 if primary (HAProxy compat) |
| `/replica` | GET | No | 200 if replica (HAProxy compat) |
| `/raft/state` | GET | No | Raft leader info |
| `/cluster/failover` | POST | **Yes** | Manual failover |
| `/cluster/switchover` | POST | **Yes** | Graceful switchover |
| `/raft/add-peer` | POST | **Yes** | Add cluster node |
| `/raft/remove-peer` | POST | **Yes** | Remove cluster node |

---

## Architecture

**Agent-based failover:** Raft leader publishes directives (FENCE, PROMOTE, DEMOTE). Each node executes its own directives locally via pg_ctl. No SSH/remote execution needed.

**Node states:** `INITIALIZING` → `RUNNING_PRIMARY`/`RUNNING_REPLICA` → `PROMOTING`/`DEMOTING`/`FENCED`

**Key interfaces:**
- `ClusterStateSource` — Router reads from Raft FSM
- `LeadershipSource` — VIP uses Raft for leader election

---

## Build & Test

```bash
# Build
go build -o pgbastion ./cmd/pgbastion/

# Unit tests
go test ./internal/...

# E2E tests (requires Docker)
cd test/e2e && ./run.sh
```

---

## Coding Conventions

- **Logging:** `log/slog` with component child loggers
- **Errors:** Wrap with `fmt.Errorf("doing X: %w", err)`
- **Concurrency:** Channels for signals, mutexes for shared state
- **Imports:** stdlib → external → internal (blank line separated)

---

## Production Readiness Checklist

### Sprint 1: Security (CRITICAL) — COMPLETE
- [x] **API Authentication** — Bearer token auth for mutation endpoints
- [x] **API Bind Address** — Config option to bind to specific interface (default 127.0.0.1)
- [x] **Command Validation** — Validate postgresql.bin_dir paths (absolute, no shell chars)

### Sprint 2: Stability — COMPLETE
- [x] Health check exponential backoff on failures
- [x] Configurable Raft parameters (timeouts, snapshot retention)
- [x] Bounded timeout on VIP resign during shutdown
- [x] Request correlation IDs in logs (X-Request-ID header)

### Sprint 3: Operations — COMPLETE
- [x] Additional Prometheus metrics (Raft term, consensus latency, API request duration)
- [x] Config schema versioning (`version: 1` with validation)
- [ ] Chaos/error path test coverage (deferred)
- [x] Operational runbooks and troubleshooting docs (OPERATIONS.md)

### Future
- [ ] Web UI dashboard
- [ ] Alert integrations (Slack, PagerDuty)
- [ ] Connection pooling
- [ ] Distributed tracing

---

## Recent Changes

**2026-02-15:**
- **Sprint 3 Operations Complete:**
  - Prometheus metrics: `pgbastion_raft_state`, `pgbastion_raft_term`, `pgbastion_raft_peers_count`, `pgbastion_raft_is_leader`, `pgbastion_api_request_duration_seconds`, `pgbastion_api_requests_total`
  - Config schema versioning (`version: 1` field with validation for forward/backward compatibility)
  - Created `OPERATIONS.md` with cluster bootstrap, node management, failover/switchover procedures, disaster recovery, troubleshooting, and monitoring guidance
- **Sprint 2 Stability Complete:**
  - Health check exponential backoff (`postgresql.health_interval`, `health_max_backoff`, `health_backoff_multiplier`)
  - Configurable Raft parameters (`raft.log_level`, `snapshot_retain`, `transport_timeout`, `heartbeat_timeout`, `election_timeout`)
  - Bounded 5s timeout on VIP resign during shutdown (prevents hanging)
  - Request correlation IDs (`X-Request-ID` header, `request_id` in all logs)
- **Sprint 1 Security Complete:**
  - Bearer token authentication for mutation endpoints (`api.auth_token`)
  - Configurable API bind address (`api.bind_address`, defaults to 127.0.0.1)
  - Path validation for `postgresql.bin_dir` and `postgresql.data_dir`
- Fixed e2e tests, double WriteHeader, pre-initialized metrics
- All 15 e2e tests passing
