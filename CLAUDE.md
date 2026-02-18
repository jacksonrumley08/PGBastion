# PGBastion — Project Reference

Standalone PostgreSQL HA daemon replacing Patroni + etcd + HAProxy + Keepalived.

## Overview

| Component | Replaces | Package |
|-----------|----------|---------|
| TCP Proxy | HAProxy | `internal/proxy/` |
| VIP Management | Keepalived | `internal/vip/` |
| Consensus | etcd | `internal/consensus/` |
| Cluster Manager | Patroni | `internal/cluster/` |

## Repository Structure

```
cmd/pgbastion/main.go     # CLI + daemon orchestration
internal/
  api/                    # REST API + Prometheus /metrics + embedded UI
  cluster/                # Health checks, failover, replication
  config/                 # YAML loading, validation, hot-reload
  consensus/              # Raft FSM, store, directives
  proxy/                  # TCP proxy, routing, connection pooling
  testutil/               # Chaos testing utilities
  tracing/                # OpenTelemetry distributed tracing
  vip/                    # VIP management via netlink
test/e2e/                 # Docker-based E2E tests
```

## Architecture

**Agent-based failover:** Raft leader publishes directives (FENCE, PROMOTE, DEMOTE). Each node executes its own directives locally via pg_ctl. No SSH/remote execution needed.

**Node states:** `INITIALIZING` → `RUNNING_PRIMARY`/`RUNNING_REPLICA` → `PROMOTING`/`DEMOTING`/`FENCED`

**Key interfaces:**
- `ClusterStateSource` — Router reads from Raft FSM
- `LeadershipSource` — VIP uses Raft for leader election

## Build & Test

```bash
go build -o pgbastion ./cmd/pgbastion/
go test ./internal/...
cd test/e2e && ./run.sh  # requires Docker
```

## Coding Conventions

- **Logging:** `log/slog` with component child loggers
- **Errors:** Wrap with `fmt.Errorf("doing X: %w", err)`
- **Concurrency:** Channels for signals, mutexes for shared state
- **Imports:** stdlib → external → internal (blank line separated)

## Configuration

Default: `/etc/pgbastion/pgbastion.yaml` — See `internal/config/config.go` for full schema.

| Port | Purpose |
|------|---------|
| 5432 | Primary proxy (writes) |
| 5433 | Replica proxy (reads) |
| 8080 | REST API |
| 9090 | Prometheus metrics |
| 8300 | Raft consensus |

### Key Settings

- `failover.fencing_enabled` — Defaults to `true`. Publishes FENCE directive to old primary before PROMOTE.
- `proxy.pool.enabled` — Enable connection pooling for backend connections.
- `raft.tls_enabled` — Enable TLS for Raft transport.
- `raft.tls_cert`, `raft.tls_key`, `raft.tls_ca` — TLS certificate paths (CA enables mTLS).
- `failover.split_brain_protection` — Defaults to `true`. Primary self-fences when quorum is lost.
- `failover.quorum_loss_timeout` — Time without quorum before self-fencing (default 30s).

### Operational Endpoints

- `GET /directives` — List all directives (alias for `/raft/directives`)
- `GET /members` — Raft cluster members (alias for `/raft/peers`)
- `GET /nodes` — All PostgreSQL nodes with health/lag data
- `GET /cluster/postgresql` — Current cluster-wide PostgreSQL config
- `PATCH /cluster/postgresql` — Update PostgreSQL parameters (requires auth)
- `DELETE /cluster/postgresql?name=param` — Reset a parameter (requires auth)

### Cluster-Wide PostgreSQL Configuration

Similar to Patroni, PGBastion manages PostgreSQL configuration via Raft consensus:

1. **API writes** go through Raft leader → FSM stores config with version
2. **ConfigWatcher** on each node polls FSM every 10s for config changes
3. **ConfigApplier** applies via `ALTER SYSTEM SET`, then `pg_reload_conf()`
4. Parameters requiring restart (context=postmaster) are tracked as `pending_restart`

Example:
```bash
# Set parameters
curl -X PATCH http://localhost:8080/cluster/postgresql \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"parameters": {"max_connections": "200", "shared_buffers": "1GB"}}'

# View config
curl http://localhost:8080/cluster/postgresql

# Reset a parameter
curl -X DELETE "http://localhost:8080/cluster/postgresql?name=max_connections" \
  -H "Authorization: Bearer $TOKEN"
```

## Remaining Work

### Future Enhancements

- [ ] **Alert Integrations** — Slack, PagerDuty webhooks on state transitions (if needed)

## Completed

### Core Functionality
- Directive execution via `Manager.processDirectives()` — polls FSM, executes pg_ctl locally
- Fencing enabled by default — FENCE directive stops old primary before PROMOTE
- Directive timeout (60s) via `FailoverController.MonitorDirectives()`
- Connection pooling integrated into proxy — uses `PoolManager` when `pool.enabled=true`
- Graceful switchover with sync — waits for replica to catch up before demote/promote
- **Cluster-wide PostgreSQL config** — Patroni-style config via Raft FSM (`/cluster/postgresql` API)

### Security & Stability
- Bearer token auth for mutation endpoints
- API bind address config (default 127.0.0.1)
- Path validation for postgresql.bin_dir/data_dir
- Health check exponential backoff
- Configurable Raft parameters
- VIP resign timeout (5s)
- Request correlation IDs
- **Raft mTLS support** — `raft.tls_enabled` with optional CA for mutual TLS
- **Split-brain protection** — Primary self-fences when quorum lost for `quorum_loss_timeout` (default 30s)

### Observability
- Prometheus metrics (31 total)
- OpenTelemetry tracing
- Web UI dashboard at `/ui/` with failover/switchover action buttons
- Server-Sent Events at `/events` for real-time updates
- Operational API endpoints (`/directives`, `/members`, `/nodes`)

### Testing
- Comprehensive chaos tests in `internal/cluster/manager_chaos_test.go`
- Tests cover: directive execution (FENCE, PROMOTE, DEMOTE, CHECKPOINT), split-brain self-fencing, command timeouts, intermittent failures, high latency, concurrent directives, Raft backoff

### Infrastructure
- Config schema versioning
- Periodic replication slot cleanup (hourly on primary)
- Node health/lag data synced to FSM
- Gratuitous ARP on VIP acquisition (updates network switch MAC tables)
- 15 E2E tests passing
- **Docker support** — Multi-stage Dockerfile with health checks
- **Raft backup/restore docs** — Comprehensive backup strategy in OPERATIONS.md
- **Upgrade documentation** — Rolling upgrade, blue-green, and rollback procedures
