// PGBastion Dashboard Application

const REFRESH_INTERVAL = 2000; // 2 seconds
const WS_RECONNECT_DELAY = 3000; // 3 seconds
let refreshTimer = null;
let isRefreshing = true;
let websocket = null;
let wsReconnectTimer = null;
let currentClusterState = null;
let currentAction = null;

// Initialize dashboard
document.addEventListener('DOMContentLoaded', () => {
    fetchAllData();
    startAutoRefresh();
    initWebSocket();
    initActionButtons();

    // Toggle refresh on click
    document.getElementById('refresh-status').addEventListener('click', toggleRefresh);
});

// Server-Sent Events for real-time updates
let eventSource = null;

function initWebSocket() {
    // Using SSE instead of WebSocket for simplicity
    initSSE();
}

function initSSE() {
    if (eventSource) {
        eventSource.close();
    }

    try {
        eventSource = new EventSource('/events');

        eventSource.onopen = () => {
            console.log('SSE connected');
            updateConnectionStatus('connected');
        };

        eventSource.onmessage = (event) => {
            try {
                const data = JSON.parse(event.data);
                handleSSEMessage(data);
            } catch (e) {
                console.error('Failed to parse SSE message:', e);
            }
        };

        eventSource.onerror = (error) => {
            console.error('SSE error:', error);
            updateConnectionStatus('disconnected');
            // EventSource will auto-reconnect
        };
    } catch (e) {
        console.log('SSE not available, using polling only');
    }
}

function handleSSEMessage(data) {
    if (data.type === 'cluster_update') {
        // Update cluster state from SSE
        if (data.cluster_state) {
            currentClusterState = data.cluster_state;
        }

        // Update relevant UI sections if data is present
        if (data.cluster && data.cluster.routing_table) {
            updateClusterTopology(data.cluster, data.cluster_state);
        }
        if (data.raft) {
            updateRaftStateFromSSE(data.raft);
        }
        if (data.health) {
            updateHealthFromSSE(data.health);
        }

        // Update action buttons based on new state
        updateActionButtonsFromSSE(data);
    }
}

function updateRaftStateFromSSE(raft) {
    document.getElementById('raft-state').textContent = raft.state || '-';
    document.getElementById('raft-leader').textContent = raft.leader || '-';
}

function updateHealthFromSSE(health) {
    const indicator = document.getElementById('status-indicator');
    indicator.className = 'status-indicator';
    if (!health.healthy) {
        indicator.classList.add('error');
    }

    // Update replication lag if in recovery
    if (health.is_in_recovery && health.lag !== undefined) {
        document.getElementById('replication-lag').textContent = formatBytes(health.lag);
    }
}

function updateActionButtonsFromSSE(data) {
    const btnSwitchover = document.getElementById('btn-switchover');
    const btnFailover = document.getElementById('btn-failover');

    const isLeader = data.raft && data.raft.is_leader;
    const hasReplicas = data.cluster_state && data.cluster_state.nodes &&
        Object.values(data.cluster_state.nodes).some(n => n.role === 'replica');

    btnSwitchover.disabled = !isLeader || !hasReplicas;
    btnFailover.disabled = !isLeader || !hasReplicas;
}

function updateConnectionStatus(status) {
    console.log('Connection status:', status);
}

// Action buttons
function initActionButtons() {
    const btnSwitchover = document.getElementById('btn-switchover');
    const btnFailover = document.getElementById('btn-failover');
    const modal = document.getElementById('action-modal');
    const modalCancel = document.getElementById('modal-cancel');
    const modalConfirm = document.getElementById('modal-confirm');

    btnSwitchover.addEventListener('click', () => showActionModal('switchover'));
    btnFailover.addEventListener('click', () => showActionModal('failover'));
    modalCancel.addEventListener('click', hideActionModal);
    modalConfirm.addEventListener('click', executeAction);

    // Close modal on background click
    modal.addEventListener('click', (e) => {
        if (e.target === modal) hideActionModal();
    });
}

function showActionModal(action) {
    currentAction = action;
    const modal = document.getElementById('action-modal');
    const modalTitle = document.getElementById('modal-title');
    const modalMessage = document.getElementById('modal-message');
    const targetSelect = document.getElementById('target-node');
    const modalStatus = document.getElementById('modal-status');

    modalStatus.textContent = '';
    modalStatus.className = 'modal-status';

    if (action === 'switchover') {
        modalTitle.textContent = 'Confirm Switchover';
        modalMessage.textContent = 'Graceful switchover will safely transfer primary role to a replica.';
    } else {
        modalTitle.textContent = 'Confirm Failover';
        modalMessage.textContent = 'WARNING: Failover will force a primary change. Use only when primary is unreachable.';
    }

    // Populate target nodes
    targetSelect.innerHTML = '<option value="">Select target node...</option>';
    if (currentClusterState && currentClusterState.nodes) {
        Object.entries(currentClusterState.nodes).forEach(([name, node]) => {
            if (node.role !== 'primary') {
                const option = document.createElement('option');
                option.value = name;
                option.textContent = `${name} (${node.state || 'unknown'})`;
                targetSelect.appendChild(option);
            }
        });
    }

    modal.classList.remove('hidden');
}

function hideActionModal() {
    const modal = document.getElementById('action-modal');
    modal.classList.add('hidden');
    currentAction = null;
    document.getElementById('auth-token').value = '';
}

async function executeAction() {
    const targetNode = document.getElementById('target-node').value;
    const authToken = document.getElementById('auth-token').value;
    const modalStatus = document.getElementById('modal-status');

    if (!targetNode) {
        modalStatus.textContent = 'Please select a target node';
        modalStatus.className = 'modal-status error';
        return;
    }

    if (!authToken) {
        modalStatus.textContent = 'Please enter the auth token';
        modalStatus.className = 'modal-status error';
        return;
    }

    modalStatus.textContent = 'Executing...';
    modalStatus.className = 'modal-status loading';

    const endpoint = currentAction === 'switchover'
        ? '/cluster/switchover'
        : '/cluster/failover';

    try {
        const response = await fetch(endpoint, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Authorization': `Bearer ${authToken}`
            },
            body: JSON.stringify({ target: targetNode })
        });

        const result = await response.json();

        if (response.ok) {
            modalStatus.textContent = result.message || 'Operation initiated successfully';
            modalStatus.className = 'modal-status success';
            setTimeout(() => {
                hideActionModal();
                fetchAllData();
            }, 2000);
        } else {
            modalStatus.textContent = result.error || `Failed: ${response.status}`;
            modalStatus.className = 'modal-status error';
        }
    } catch (error) {
        modalStatus.textContent = `Error: ${error.message}`;
        modalStatus.className = 'modal-status error';
    }
}

function showNotification(message, type) {
    // Simple notification - could be enhanced with a toast system
    console.log(`[${type}] ${message}`);
}

function startAutoRefresh() {
    refreshTimer = setInterval(fetchAllData, REFRESH_INTERVAL);
    document.getElementById('refresh-status').textContent = 'Auto-refresh: ON';
    document.getElementById('refresh-status').classList.remove('paused');
    isRefreshing = true;
}

function stopAutoRefresh() {
    clearInterval(refreshTimer);
    document.getElementById('refresh-status').textContent = 'Auto-refresh: OFF (click to resume)';
    document.getElementById('refresh-status').classList.add('paused');
    isRefreshing = false;
}

function toggleRefresh() {
    if (isRefreshing) {
        stopAutoRefresh();
    } else {
        startAutoRefresh();
        fetchAllData();
    }
}

async function fetchAllData() {
    try {
        const [health, cluster, connections, raftState, clusterState, healthHistory] = await Promise.all([
            fetchJSON('/health'),
            fetchJSON('/cluster'),
            fetchJSON('/connections'),
            fetchJSON('/raft/state').catch(() => null),
            fetchJSON('/cluster/state').catch(() => null),
            fetchJSON('/cluster/health/history').catch(() => null)
        ]);

        // Store cluster state for action modals
        currentClusterState = clusterState;

        updateStatusIndicator(health);
        updateClusterTopology(cluster, clusterState);
        updateNodeStatus(health, clusterState);
        updateConnections(connections);
        updateRaftState(raftState);
        updateHealthHistory(healthHistory);
        updateReplication(health, clusterState);
        updateActionButtons(health, clusterState, raftState);
        updateLastUpdated();

        // Check for split brain
        if (cluster && cluster.routing_table && cluster.routing_table.split_brain) {
            showSplitBrainWarning();
        } else {
            hideSplitBrainWarning();
        }
    } catch (error) {
        console.error('Failed to fetch data:', error);
        setErrorState();
    }
}

function updateActionButtons(health, clusterState, raftState) {
    const btnSwitchover = document.getElementById('btn-switchover');
    const btnFailover = document.getElementById('btn-failover');

    // Enable buttons only if:
    // 1. This node is the Raft leader (can initiate operations)
    // 2. There are replicas available
    const isLeader = raftState && raftState.state === 'Leader';
    const hasReplicas = clusterState && clusterState.nodes &&
        Object.values(clusterState.nodes).some(n => n.role === 'replica');

    btnSwitchover.disabled = !isLeader || !hasReplicas;
    btnFailover.disabled = !isLeader || !hasReplicas;

    // Update button titles for context
    if (!isLeader) {
        btnSwitchover.title = 'Only available on Raft leader';
        btnFailover.title = 'Only available on Raft leader';
    } else if (!hasReplicas) {
        btnSwitchover.title = 'No replicas available';
        btnFailover.title = 'No replicas available';
    } else {
        btnSwitchover.title = 'Graceful switchover to a replica';
        btnFailover.title = 'Force failover (use when primary is down)';
    }
}

async function fetchJSON(endpoint) {
    const response = await fetch(endpoint);
    if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
    }
    return response.json();
}

function updateStatusIndicator(health) {
    const indicator = document.getElementById('status-indicator');
    indicator.className = 'status-indicator';

    if (!health || health.status !== 'ok') {
        indicator.classList.add('error');
    } else if (health.split_brain) {
        indicator.classList.add('warning');
    }
}

function updateClusterTopology(cluster, clusterState) {
    const svg = document.getElementById('topology-svg');
    if (!cluster || !cluster.routing_table) {
        svg.innerHTML = '<text x="50%" y="50%" text-anchor="middle" fill="#a0a0a0">No cluster data</text>';
        return;
    }

    const rt = cluster.routing_table;
    const nodes = [];

    // Add primary
    if (rt.primary) {
        nodes.push({ address: rt.primary, role: 'primary' });
    }

    // Add replicas
    if (rt.replicas) {
        rt.replicas.forEach(addr => {
            nodes.push({ address: addr, role: 'replica' });
        });
    }

    if (nodes.length === 0) {
        svg.innerHTML = '<text x="50%" y="50%" text-anchor="middle" fill="#a0a0a0">No nodes available</text>';
        return;
    }

    const width = svg.clientWidth || 400;
    const height = 200;
    const centerX = width / 2;
    const centerY = height / 2;
    const radius = Math.min(width, height) / 3;

    let svgContent = '';

    // Draw connections from primary to replicas
    const primary = nodes.find(n => n.role === 'primary');
    if (primary) {
        const primaryIndex = nodes.indexOf(primary);
        const primaryAngle = (2 * Math.PI * primaryIndex) / nodes.length - Math.PI / 2;
        const primaryX = centerX + (nodes.length > 1 ? radius : 0) * Math.cos(primaryAngle);
        const primaryY = centerY + (nodes.length > 1 ? radius : 0) * Math.sin(primaryAngle);

        nodes.forEach((node, i) => {
            if (node.role === 'replica') {
                const angle = (2 * Math.PI * i) / nodes.length - Math.PI / 2;
                const x = centerX + radius * Math.cos(angle);
                const y = centerY + radius * Math.sin(angle);
                svgContent += `<line class="replication-line" x1="${primaryX}" y1="${primaryY}" x2="${x}" y2="${y}"/>`;
            }
        });
    }

    // Draw nodes
    nodes.forEach((node, i) => {
        const angle = (2 * Math.PI * i) / nodes.length - Math.PI / 2;
        const x = centerX + (nodes.length > 1 ? radius : 0) * Math.cos(angle);
        const y = centerY + (nodes.length > 1 ? radius : 0) * Math.sin(angle);

        const nodeClass = node.role === 'primary' ? 'node-primary' : 'node-replica';
        svgContent += `
            <g class="node-circle ${nodeClass}" transform="translate(${x}, ${y})">
                <circle r="25" />
                <text class="node-label" dy="4">${node.role === 'primary' ? 'P' : 'R'}</text>
            </g>
            <text x="${x}" y="${y + 40}" class="node-label" fill="#a0a0a0" font-size="10">${formatAddress(node.address)}</text>
        `;
    });

    svg.innerHTML = svgContent;
}

function updateNodeStatus(health, clusterState) {
    const container = document.getElementById('nodes');

    if (!health) {
        container.innerHTML = '<div class="error-message">Unable to fetch node status</div>';
        return;
    }

    let html = '';

    // Current node card
    const cardClass = health.status !== 'ok' ? 'unhealthy' : (health.role === 'primary' ? 'primary' : '');
    html += `
        <div class="node-card ${cardClass}">
            <div class="node-card-header">
                <span class="node-name">${health.node || 'Unknown'}</span>
                <span class="node-role ${health.role === 'primary' ? 'primary' : ''}">${health.role || '-'}</span>
            </div>
            <div class="node-details">
                <div class="node-detail">
                    <span>Status</span>
                    <span>${health.status || '-'}</span>
                </div>
                <div class="node-detail">
                    <span>VIP Owner</span>
                    <span>${health.vip_owner ? 'Yes' : 'No'}</span>
                </div>
            </div>
        </div>
    `;

    // Add other nodes from cluster state if available
    if (clusterState && clusterState.nodes) {
        Object.entries(clusterState.nodes).forEach(([name, node]) => {
            if (name === health.node) return; // Skip current node

            const nodeClass = node.role === 'primary' ? 'primary' : '';
            html += `
                <div class="node-card ${nodeClass}">
                    <div class="node-card-header">
                        <span class="node-name">${name}</span>
                        <span class="node-role ${node.role === 'primary' ? 'primary' : ''}">${node.role || '-'}</span>
                    </div>
                    <div class="node-details">
                        <div class="node-detail">
                            <span>State</span>
                            <span>${node.state || '-'}</span>
                        </div>
                        <div class="node-detail">
                            <span>Lag</span>
                            <span>${formatBytes(node.lag)}</span>
                        </div>
                    </div>
                </div>
            `;
        });
    }

    container.innerHTML = html;
}

function updateConnections(connections) {
    if (!connections) return;

    document.getElementById('active-connections').textContent = connections.active_connections || 0;

    const byBackend = document.getElementById('connections-by-backend');
    if (connections.by_backend && Object.keys(connections.by_backend).length > 0) {
        let html = '';
        Object.entries(connections.by_backend).forEach(([backend, count]) => {
            html += `
                <div class="backend-connection">
                    <span>${formatAddress(backend)}</span>
                    <span>${count}</span>
                </div>
            `;
        });
        byBackend.innerHTML = html;
    } else {
        byBackend.innerHTML = '<div class="backend-connection"><span>No active connections</span></div>';
    }
}

function updateRaftState(raftState) {
    if (!raftState) {
        document.getElementById('raft-state').textContent = '-';
        document.getElementById('raft-term').textContent = '-';
        document.getElementById('raft-leader').textContent = '-';
        document.getElementById('raft-peers').textContent = '-';
        return;
    }

    document.getElementById('raft-state').textContent = raftState.state || '-';
    document.getElementById('raft-term').textContent = raftState.term || '-';
    document.getElementById('raft-leader').textContent = raftState.leader || '-';
    document.getElementById('raft-peers').textContent = raftState.peers ? raftState.peers.length : '-';
}

function updateHealthHistory(healthHistory) {
    const container = document.querySelector('.health-timeline');

    if (!healthHistory || !healthHistory.entries || healthHistory.entries.length === 0) {
        container.innerHTML = '<span style="color: #a0a0a0; font-size: 0.85rem;">No health history available</span>';
        document.getElementById('health-success-rate').textContent = '';
        document.getElementById('health-avg-duration').textContent = '';
        return;
    }

    const entries = healthHistory.entries.slice(-50); // Last 50 entries
    const maxDuration = Math.max(...entries.map(e => e.duration_ms || 0), 1);

    let html = '';
    entries.forEach(entry => {
        const height = Math.max(10, (entry.duration_ms / maxDuration) * 100);
        const className = entry.status && entry.status.healthy ? '' : 'failure';
        html += `<div class="health-bar ${className}" style="height: ${height}%" title="${entry.duration_ms}ms"></div>`;
    });
    container.innerHTML = html;

    // Update stats
    if (healthHistory.stats) {
        const stats = healthHistory.stats;
        document.getElementById('health-success-rate').textContent =
            `Success rate: ${(stats.success_rate || 0).toFixed(1)}%`;
        document.getElementById('health-avg-duration').textContent =
            `Avg duration: ${(stats.avg_duration_ms || 0).toFixed(1)}ms`;
    }
}

function updateReplication(health, clusterState) {
    const lagElement = document.getElementById('replication-lag');
    const replicasList = document.getElementById('replicas-list');

    // Show replication lag for replica nodes
    if (health && health.health_status && health.health_status.replication_lag_bytes !== undefined) {
        const lag = health.health_status.replication_lag_bytes;
        lagElement.textContent = formatBytes(lag);
        lagElement.className = 'stat-value' + (lag > 1000000 ? ' warning' : '');
    } else {
        lagElement.textContent = 'N/A';
    }

    // Show replicas from primary perspective
    if (health && health.health_status && health.health_status.replicas) {
        let html = '';
        health.health_status.replicas.forEach(replica => {
            const lagClass = (replica.replay_lag_bytes || 0) > 1000000 ? '' : 'ok';
            html += `
                <div class="replica-item">
                    <span>${replica.name || 'Unknown'}</span>
                    <span class="replica-lag ${lagClass}">${formatBytes(replica.replay_lag_bytes || 0)}</span>
                </div>
            `;
        });
        replicasList.innerHTML = html || '<div class="replica-item">No replicas connected</div>';
    } else {
        replicasList.innerHTML = '';
    }
}

function updateLastUpdated() {
    document.getElementById('last-updated').textContent =
        `Last updated: ${new Date().toLocaleTimeString()}`;
}

function setErrorState() {
    document.getElementById('status-indicator').classList.add('error');
    document.getElementById('last-updated').textContent =
        `Last updated: ${new Date().toLocaleTimeString()} (error)`;
}

function showSplitBrainWarning() {
    if (!document.querySelector('.split-brain-warning')) {
        const warning = document.createElement('div');
        warning.className = 'split-brain-warning';
        warning.textContent = 'SPLIT-BRAIN DETECTED - Routing disabled for safety';
        document.body.insertBefore(warning, document.querySelector('main'));
    }
}

function hideSplitBrainWarning() {
    const warning = document.querySelector('.split-brain-warning');
    if (warning) {
        warning.remove();
    }
}

// Utility functions
function formatAddress(address) {
    if (!address) return '-';
    // Shorten address for display
    const parts = address.split(':');
    if (parts.length === 2) {
        const host = parts[0];
        const port = parts[1];
        // Show last octet and port
        const hostParts = host.split('.');
        if (hostParts.length === 4) {
            return `...${hostParts[3]}:${port}`;
        }
    }
    return address.length > 20 ? address.substring(0, 17) + '...' : address;
}

function formatBytes(bytes) {
    if (bytes === undefined || bytes === null) return '-';
    if (bytes === 0) return '0 B';

    const units = ['B', 'KB', 'MB', 'GB'];
    const i = Math.min(Math.floor(Math.log(Math.abs(bytes)) / Math.log(1024)), units.length - 1);
    return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`;
}
