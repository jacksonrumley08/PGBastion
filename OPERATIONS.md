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
7. [Raft State Backup and Restore](#raft-state-backup-and-restore)
8. [Disaster Recovery](#disaster-recovery)
9. [Troubleshooting](#troubleshooting)
10. [Monitoring](#monitoring)
11. [Upgrading PGBastion](#upgrading-pgbastion)

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

## Raft State Backup and Restore

### What's in the Raft Data Directory

The Raft data directory (`raft.data_dir`, typically `/var/lib/pgbastion/raft`) contains:

| File/Directory | Purpose |
|----------------|---------|
| `raft.db` | BoltDB database with Raft log entries and configuration |
| `snapshots/` | FSM state snapshots (compressed, periodic) |
| `stable.db` | Persistent node state (current term, voted for) |

**Important:** The Raft state is automatically replicated across all cluster nodes. Under normal operation, you don't need to restore from backup — just re-add the node to the cluster and it will sync automatically.

Backups are only needed for:
- Complete cluster loss (all nodes destroyed)
- Compliance/audit requirements
- Point-in-time recovery of cluster metadata

### Backup Strategy

#### Option 1: Filesystem Backup (Recommended)

```bash
# Stop pgbastion before backup to ensure consistency
systemctl stop pgbastion

# Create backup
BACKUP_DIR="/backup/pgbastion/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"
cp -a /var/lib/pgbastion/raft "$BACKUP_DIR/"
cp /etc/pgbastion/pgbastion.yaml "$BACKUP_DIR/"

# Restart pgbastion
systemctl start pgbastion

# Verify backup
ls -la "$BACKUP_DIR/raft/"
```

#### Option 2: Live Backup (Using Snapshots)

If you can't stop PGBastion, use the Raft snapshot feature:

```bash
# Trigger a snapshot (creates consistent point-in-time copy)
# Snapshots are automatically created periodically, but you can force one
# by checking the snapshots directory

BACKUP_DIR="/backup/pgbastion/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"

# Copy the latest snapshot (most consistent for live backup)
LATEST_SNAP=$(ls -td /var/lib/pgbastion/raft/snapshots/*/ 2>/dev/null | head -1)
if [ -n "$LATEST_SNAP" ]; then
    cp -a "$LATEST_SNAP" "$BACKUP_DIR/snapshot/"
fi

# Also backup config
cp /etc/pgbastion/pgbastion.yaml "$BACKUP_DIR/"
```

#### Automated Backup Script

```bash
#!/bin/bash
# /usr/local/bin/pgbastion-backup.sh

set -e

BACKUP_ROOT="/backup/pgbastion"
RETENTION_DAYS=7
RAFT_DIR="/var/lib/pgbastion/raft"
CONFIG_FILE="/etc/pgbastion/pgbastion.yaml"

BACKUP_DIR="$BACKUP_ROOT/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"

# Backup Raft state
cp -a "$RAFT_DIR" "$BACKUP_DIR/"

# Backup config
cp "$CONFIG_FILE" "$BACKUP_DIR/"

# Create checksum
cd "$BACKUP_DIR"
sha256sum -b raft/* > checksums.sha256 2>/dev/null || true

# Compress
tar -czf "$BACKUP_DIR.tar.gz" -C "$BACKUP_ROOT" "$(basename $BACKUP_DIR)"
rm -rf "$BACKUP_DIR"

# Cleanup old backups
find "$BACKUP_ROOT" -name "*.tar.gz" -mtime +$RETENTION_DAYS -delete

echo "Backup completed: $BACKUP_DIR.tar.gz"
```

Add to cron for daily backups:
```bash
# /etc/cron.d/pgbastion-backup
0 2 * * * root /usr/local/bin/pgbastion-backup.sh >> /var/log/pgbastion-backup.log 2>&1
```

### Restore Procedures

#### Scenario 1: Single Node Recovery (Cluster Still Healthy)

If one node is lost but the cluster still has quorum, **don't restore from backup**. Instead:

```bash
# On the recovered/new node:

# 1. Ensure clean Raft state
rm -rf /var/lib/pgbastion/raft/*

# 2. Set bootstrap: false in config
# Edit /etc/pgbastion/pgbastion.yaml
#   raft:
#     bootstrap: false

# 3. Start pgbastion
systemctl start pgbastion

# 4. Add node back to cluster (from leader)
curl -X POST http://leader:8080/raft/add-peer \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id": "recovered-node", "address": "10.0.1.3:8300"}'
```

The node will automatically sync state from the cluster.

#### Scenario 2: Complete Cluster Loss (Restore from Backup)

If all nodes are lost, restore from backup on one node:

```bash
# On the recovery node:

# 1. Stop pgbastion if running
systemctl stop pgbastion

# 2. Extract backup
BACKUP_FILE="/backup/pgbastion/20260217-020000.tar.gz"
tar -xzf "$BACKUP_FILE" -C /tmp/
cp -a /tmp/*/raft /var/lib/pgbastion/

# 3. Verify checksum (if available)
cd /var/lib/pgbastion
sha256sum -c /tmp/*/checksums.sha256

# 4. Set ownership
chown -R pgbastion:pgbastion /var/lib/pgbastion/raft

# 5. Configure as bootstrap node
# Edit /etc/pgbastion/pgbastion.yaml:
#   raft:
#     bootstrap: true  # Force re-bootstrap from restored state

# 6. Start pgbastion
systemctl start pgbastion

# 7. Verify recovery
curl -s http://127.0.0.1:8080/raft/state | jq
curl -s http://127.0.0.1:8080/cluster | jq

# 8. Add other nodes back to cluster
curl -X POST http://127.0.0.1:8080/raft/add-peer \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -d '{"id": "node2", "address": "10.0.1.2:8300"}'
```

#### Scenario 3: Point-in-Time Recovery

To recover cluster state from a specific point in time:

```bash
# 1. Stop all pgbastion instances in the cluster
# On each node:
systemctl stop pgbastion

# 2. Choose the backup to restore
ls -la /backup/pgbastion/

# 3. Restore to the designated leader node
BACKUP_FILE="/backup/pgbastion/20260215-020000.tar.gz"
rm -rf /var/lib/pgbastion/raft/*
tar -xzf "$BACKUP_FILE" -C /tmp/
cp -a /tmp/*/raft/* /var/lib/pgbastion/raft/
chown -R pgbastion:pgbastion /var/lib/pgbastion/raft

# 4. Clear Raft data on other nodes (they will re-sync)
# On node2, node3, etc:
rm -rf /var/lib/pgbastion/raft/*

# 5. Start the restored leader (bootstrap: true)
systemctl start pgbastion

# 6. Start other nodes (bootstrap: false) and re-add them
```

### Backup Best Practices

1. **Backup frequency:** Daily minimum, hourly for critical clusters
2. **Retention:** Keep at least 7 days of backups
3. **Test restores:** Quarterly restore tests to verify backup integrity
4. **Off-site copies:** Replicate backups to different location/region
5. **Monitor backup jobs:** Alert on backup failures
6. **Include config:** Always backup `pgbastion.yaml` alongside Raft state

### What Backups Do NOT Include

Raft backups contain cluster metadata only:
- Which node is primary
- Health check history
- PostgreSQL configuration (managed by PGBastion)
- Directive history

They do **NOT** include:
- PostgreSQL data (backup separately using pg_basebackup/pgBackRest)
- PostgreSQL WAL files
- Application data

**Always maintain separate PostgreSQL backups!**

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

---

## Upgrading PGBastion

### Version Compatibility

PGBastion follows semantic versioning (MAJOR.MINOR.PATCH):
- **PATCH** (1.0.x → 1.0.y): Bug fixes, no config changes. Rolling upgrade safe.
- **MINOR** (1.x.0 → 1.y.0): New features, backward-compatible config. Rolling upgrade safe.
- **MAJOR** (x.0.0 → y.0.0): Breaking changes possible. Check release notes carefully.

### Pre-Upgrade Checklist

```bash
# 1. Check current version on all nodes
for node in node1 node2 node3; do
  echo "$node: $(ssh $node pgbastion version)"
done

# 2. Review release notes for breaking changes
# https://github.com/yourcompany/pgbastion/releases

# 3. Backup Raft state on all nodes
for node in node1 node2 node3; do
  ssh $node /usr/local/bin/pgbastion-backup.sh
done

# 4. Verify cluster health
curl -s http://127.0.0.1:8080/health | jq
curl -s http://127.0.0.1:8080/raft/peers | jq

# 5. Check for pending directives
curl -s http://127.0.0.1:8080/raft/directives | jq
```

### Rolling Upgrade (Recommended)

Upgrade one node at a time, starting with replicas:

```bash
# 1. Identify the current leader
LEADER=$(curl -s http://127.0.0.1:8080/raft/state | jq -r '.leader_id')
echo "Current leader: $LEADER"

# 2. Upgrade replica nodes first (one at a time)
for NODE in node2 node3; do
  echo "=== Upgrading $NODE ==="

  # Stop pgbastion
  ssh $NODE "systemctl stop pgbastion"

  # Install new binary
  scp pgbastion-new $NODE:/usr/local/bin/pgbastion
  ssh $NODE "chmod +x /usr/local/bin/pgbastion"

  # Start pgbastion
  ssh $NODE "systemctl start pgbastion"

  # Wait for node to rejoin cluster
  sleep 10
  curl -s http://$NODE:8080/health | jq '.status'

  # Verify cluster still healthy
  curl -s http://127.0.0.1:8080/raft/peers | jq '.peers | length'
done

# 3. Transfer leadership away from the leader node
curl -X POST http://$LEADER:8080/raft/transfer-leadership \
  -H "Authorization: Bearer $AUTH_TOKEN"

# Wait for new leader
sleep 5
NEW_LEADER=$(curl -s http://127.0.0.1:8080/raft/state | jq -r '.leader_id')
echo "New leader: $NEW_LEADER"

# 4. Upgrade the old leader (now a follower)
ssh $LEADER "systemctl stop pgbastion"
scp pgbastion-new $LEADER:/usr/local/bin/pgbastion
ssh $LEADER "chmod +x /usr/local/bin/pgbastion"
ssh $LEADER "systemctl start pgbastion"

# 5. Verify all nodes on new version
for node in node1 node2 node3; do
  echo "$node: $(ssh $node pgbastion version)"
done
```

### Blue-Green Upgrade (Zero Downtime)

For major version upgrades with potential breaking changes:

```bash
# 1. Build new cluster alongside existing one
# - Deploy 3 new nodes with new PGBastion version
# - Configure PostgreSQL streaming replication from old to new cluster

# 2. Verify new cluster is healthy
curl -s http://new-cluster:8080/health | jq

# 3. Verify replication is caught up
curl -s http://new-cluster:8080/cluster/replication | jq

# 4. Switchover to new cluster
# - Update DNS/load balancer to point to new VIP
# - Or update application connection strings

# 5. Decommission old cluster after validation period
```

### Rollback Procedure

If upgrade causes issues:

```bash
# 1. Stop pgbastion on affected node
systemctl stop pgbastion

# 2. Restore previous binary
cp /usr/local/bin/pgbastion.backup /usr/local/bin/pgbastion

# 3. If Raft state is corrupted, restore from backup
rm -rf /var/lib/pgbastion/raft/*
tar -xzf /backup/pgbastion/latest.tar.gz -C /tmp/
cp -a /tmp/*/raft/* /var/lib/pgbastion/raft/
chown -R pgbastion:pgbastion /var/lib/pgbastion/raft

# 4. Restart pgbastion
systemctl start pgbastion

# 5. Verify recovery
curl -s http://127.0.0.1:8080/health | jq
```

### Docker/Container Upgrades

```bash
# 1. Pull new image
docker pull pgbastion:1.2.0

# 2. Rolling restart (if using orchestrator)
# Kubernetes: kubectl rollout restart deployment/pgbastion
# Docker Swarm: docker service update --image pgbastion:1.2.0 pgbastion

# 3. Manual container upgrade
docker stop pgbastion
docker rm pgbastion
docker run -d --name pgbastion \
  -v pgbastion-data:/var/lib/pgbastion \
  -v /etc/pgbastion:/etc/pgbastion:ro \
  pgbastion:1.2.0
```

### Post-Upgrade Verification

```bash
# 1. Check version on all nodes
curl -s http://127.0.0.1:8080/health | jq '.version'

# 2. Verify Raft cluster health
curl -s http://127.0.0.1:8080/raft/state | jq
curl -s http://127.0.0.1:8080/raft/peers | jq

# 3. Verify PostgreSQL cluster health
curl -s http://127.0.0.1:8080/cluster/state | jq
curl -s http://127.0.0.1:8080/cluster/replication | jq

# 4. Test failover (in non-production)
curl -X POST http://127.0.0.1:8080/cluster/switchover \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -d '{"to": "node2"}'

# 5. Check metrics are being collected
curl -s http://127.0.0.1:9090/metrics | grep pgbastion_
```
