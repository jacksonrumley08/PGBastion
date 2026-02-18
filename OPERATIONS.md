# PGBastion Operations Guide

This guide covers common operational tasks, troubleshooting, and disaster recovery procedures for PGBastion clusters.

---

## Table of Contents

1. [Cluster Bootstrap](#cluster-bootstrap)
2. [Adding a Node](#adding-a-node)
3. [Removing a Node](#removing-a-node)
4. [Manual Failover](#manual-failover)
5. [Graceful Switchover](#graceful-switchover)
6. [Pausing Auto-Failover](#pausing-auto-failover)
7. [Disaster Recovery](#disaster-recovery)
8. [Troubleshooting](#troubleshooting)
9. [Monitoring](#monitoring)

---

## Cluster Bootstrap

### First Node (Bootstrap)

```bash
# 1. Create config file
cat > /etc/pgbastion/pgbastion.yaml << 'EOF'
version: 1
node:
  name: node1
  address: 10.0.1.1

raft:
  data_dir: /var/lib/pgbastion/raft
  bind_port: 8300
  bootstrap: true  # ONLY on first node

proxy:
  primary_port: 5432
  replica_port: 5433

api:
  port: 8080
  bind_address: 127.0.0.1
  auth_token: "your-secure-token-here"

postgresql:
  bin_dir: /usr/lib/postgresql/16/bin
  data_dir: /var/lib/postgresql/16/main
  connect_dsn: "host=/var/run/postgresql dbname=postgres"
EOF

# 2. Start pgbastion
systemctl start pgbastion

# 3. Verify bootstrap
curl -s http://127.0.0.1:8080/raft/state | jq
# Should show: "state": "Leader", "is_leader": true
```

### Additional Nodes

```bash
# 1. Create config (bootstrap: false)
cat > /etc/pgbastion/pgbastion.yaml << 'EOF'
version: 1
node:
  name: node2
  address: 10.0.1.2

raft:
  data_dir: /var/lib/pgbastion/raft
  bind_port: 8300
  bootstrap: false  # Not a bootstrap node

# ... rest of config same as node1
EOF

# 2. Start pgbastion
systemctl start pgbastion

# 3. Add to cluster (run on leader node)
curl -X POST http://127.0.0.1:8080/raft/add-peer \
  -H "Authorization: Bearer your-secure-token-here" \
  -H "Content-Type: application/json" \
  -d '{"id": "node2", "address": "10.0.1.2:8300"}'

# 4. Verify peer joined
curl -s http://127.0.0.1:8080/raft/peers | jq
```

---

## Adding a Node

```bash
# On the new node: start pgbastion with bootstrap: false

# On the leader node:
curl -X POST http://127.0.0.1:8080/raft/add-peer \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id": "node3", "address": "10.0.1.3:8300"}'

# Verify
curl -s http://127.0.0.1:8080/raft/peers | jq '.peers'
```

---

## Removing a Node

```bash
# 1. Drain connections (if removing primary, do switchover first)
pgbastion switchover --to node2

# 2. Stop pgbastion on the node being removed
systemctl stop pgbastion

# 3. Remove from cluster (run on leader)
curl -X POST http://127.0.0.1:8080/raft/remove-peer \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id": "node3"}'

# 4. Clean up Raft data on removed node
rm -rf /var/lib/pgbastion/raft
```

---

## Manual Failover

Use when primary is down and auto-failover hasn't triggered (e.g., paused).

```bash
# Trigger failover to specific node
curl -X POST http://127.0.0.1:8080/cluster/failover \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"to": "node2"}'

# Monitor progress
watch -n1 'curl -s http://127.0.0.1:8080/cluster/state | jq'
```

---

## Graceful Switchover

Use for planned maintenance. Ensures clean primary transition.

```bash
# 1. Check replication lag is acceptable
curl -s http://127.0.0.1:8080/cluster/replication | jq

# 2. Trigger switchover
curl -X POST http://127.0.0.1:8080/cluster/switchover \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"to": "node2"}'

# 3. Verify new primary
curl -s http://127.0.0.1:8080/cluster/state | jq
```

---

## Pausing Auto-Failover

Use during maintenance windows to prevent unwanted failovers.

```bash
# Pause
curl -X POST http://127.0.0.1:8080/cluster/pause \
  -H "Authorization: Bearer $AUTH_TOKEN"

# Check status
curl -s http://127.0.0.1:8080/cluster/state | jq '.paused'

# Resume
curl -X POST http://127.0.0.1:8080/cluster/resume \
  -H "Authorization: Bearer $AUTH_TOKEN"
```

---

## Disaster Recovery

### Scenario: Lost Quorum (Majority of nodes down)

```bash
# 1. Stop all pgbastion instances
systemctl stop pgbastion

# 2. On the node with most recent data, force bootstrap
# Edit config: bootstrap: true

# 3. Clear Raft data on all nodes
rm -rf /var/lib/pgbastion/raft/*

# 4. Start the designated leader first
systemctl start pgbastion

# 5. Wait for leader election, then add other nodes back
curl -X POST http://127.0.0.1:8080/raft/add-peer ...
```

### Scenario: Primary Data Corruption

```bash
# 1. Pause auto-failover
curl -X POST http://127.0.0.1:8080/cluster/pause -H "Authorization: Bearer $AUTH_TOKEN"

# 2. Promote best replica
curl -X POST http://127.0.0.1:8080/cluster/failover \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -d '{"to": "node2"}'

# 3. On corrupted node, reinitialize from new primary
curl -X POST http://127.0.0.1:8080/cluster/reinitialize \
  -H "Authorization: Bearer $AUTH_TOKEN"

# 4. Resume auto-failover
curl -X POST http://127.0.0.1:8080/cluster/resume -H "Authorization: Bearer $AUTH_TOKEN"
```

---

## Troubleshooting

### Health Check Failures

**Symptom:** `/health` returns 503, logs show connection errors.

```bash
# Check PostgreSQL is running
systemctl status postgresql

# Check health check history
curl -s http://127.0.0.1:8080/cluster/health/history?limit=10 | jq

# Check PostgreSQL connectivity
psql -h /var/run/postgresql -d postgres -c "SELECT 1"
```

**Common causes:**
- PostgreSQL not running
- Incorrect `connect_dsn` in config
- pg_hba.conf blocking connections

### Raft Not Electing Leader

**Symptom:** `/raft/state` shows "Candidate" indefinitely.

```bash
# Check all peers can communicate
curl -s http://127.0.0.1:8080/raft/peers | jq

# Verify network connectivity between nodes
nc -zv 10.0.1.2 8300

# Check firewall rules
iptables -L -n | grep 8300
```

**Common causes:**
- Network partition between nodes
- Firewall blocking Raft port (8300)
- Clock skew between nodes

### Split-Brain Detection

**Symptom:** `/health` shows `split_brain: true`.

```bash
# Check which node is marked as primary
curl -s http://127.0.0.1:8080/cluster | jq '.routing_table'

# Force fencing of the old primary
curl -X POST http://127.0.0.1:8080/cluster/failover \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -d '{"to": "node2"}'
```

### VIP Not Assigned

**Symptom:** VIP address not responding.

```bash
# Check VIP ownership
curl -s http://127.0.0.1:8080/health | jq '.vip_owner'

# Check if VIP is on interface
ip addr show eth0 | grep 10.0.1.100

# Check Raft leadership (VIP follows leader)
curl -s http://127.0.0.1:8080/raft/state | jq '.is_leader'
```

**Common causes:**
- Node is not Raft leader
- Missing `CAP_NET_ADMIN` capability
- Wrong interface name in config

### High Replication Lag

**Symptom:** Replicas showing high lag in `/cluster/replication`.

```bash
# Check replication status
curl -s http://127.0.0.1:8080/cluster/replication | jq

# On PostgreSQL, check replication slots
psql -c "SELECT * FROM pg_replication_slots"

# Check WAL sender status
psql -c "SELECT * FROM pg_stat_replication"
```

---

## Monitoring

### Key Metrics to Alert On

| Metric | Alert Threshold | Description |
|--------|----------------|-------------|
| `pgbastion_raft_is_leader` | Changes | Leadership changed |
| `pgbastion_raft_peers_count` | < expected | Node left cluster |
| `pgbastion_health_check_total{result="error"}` | > 3/min | Health check failures |
| `pgbastion_replication_lag_bytes` | > 10MB | High replication lag |
| `pgbastion_api_request_duration_seconds` | p99 > 1s | Slow API responses |

### Prometheus Alert Rules Example

```yaml
groups:
  - name: pgbastion
    rules:
      - alert: PGBastionLeadershipChange
        expr: changes(pgbastion_raft_is_leader[5m]) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "Raft leadership changed"

      - alert: PGBastionNodeDown
        expr: pgbastion_raft_peers_count < 3
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Cluster has fewer than expected nodes"

      - alert: PGBastionHighReplicationLag
        expr: pgbastion_replication_lag_bytes > 10485760
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Replication lag exceeds 10MB"
```

### Grafana Dashboard

Key panels to include:
- Raft state timeline (leader/follower transitions)
- Replication lag over time
- Health check success rate
- API request latency (p50, p95, p99)
- Active connections by backend
- VIP ownership timeline

---

## Log Analysis

### Important Log Patterns

```bash
# Failover events
journalctl -u pgbastion | grep -i "failover\|promote\|demote"

# Health check issues
journalctl -u pgbastion | grep -i "health check failed"

# Raft leadership changes
journalctl -u pgbastion | grep -i "became leader\|lost leader"

# Authentication failures (security)
journalctl -u pgbastion | grep -i "invalid auth token"
```

### Correlating Events Across Nodes

Use the `request_id` field in logs to trace requests across the cluster:

```bash
# Find all logs for a specific request
journalctl -u pgbastion | grep "request_id=abc123"
```
