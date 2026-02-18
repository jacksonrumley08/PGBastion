package consensus

import (
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	raftState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgbastion_raft_state",
		Help: "Current Raft state (1 = active for that state)",
	}, []string{"state"})

	raftTerm = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_raft_term",
		Help: "Current Raft term",
	})

	raftCommitIndex = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_raft_commit_index",
		Help: "Current Raft commit index",
	})

	raftLastLogIndex = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_raft_last_log_index",
		Help: "Last Raft log index",
	})

	raftAppliedIndex = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_raft_applied_index",
		Help: "Last applied Raft log index",
	})

	raftPeersCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_raft_peers_count",
		Help: "Number of Raft peers",
	})

	raftIsLeader = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pgbastion_raft_is_leader",
		Help: "1 if this node is the Raft leader, 0 otherwise",
	})

	raftApplyLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pgbastion_raft_apply_latency_seconds",
		Help:    "Time to apply commands to Raft",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms to ~1s
	})

	raftSnapshotLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pgbastion_raft_snapshot_latency_seconds",
		Help:    "Time to take Raft snapshots",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms to ~10s
	})
)

func init() {
	// Pre-initialize state metrics.
	raftState.WithLabelValues("Leader")
	raftState.WithLabelValues("Follower")
	raftState.WithLabelValues("Candidate")
}

// MetricsCollector collects Raft metrics from a Store.
type MetricsCollector struct {
	store  *Store
	mu     sync.Mutex
	stopCh chan struct{}
}

// NewMetricsCollector creates a new Raft metrics collector.
func NewMetricsCollector(store *Store) *MetricsCollector {
	return &MetricsCollector{
		store:  store,
		stopCh: make(chan struct{}),
	}
}

// Start begins periodic metrics collection.
func (m *MetricsCollector) Start(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Initial collection.
		m.Collect()

		for {
			select {
			case <-ticker.C:
				m.Collect()
			case <-m.stopCh:
				return
			}
		}
	}()
}

// Stop stops the metrics collection.
func (m *MetricsCollector) Stop() {
	close(m.stopCh)
}

// Collect updates all Raft metrics from the store.
func (m *MetricsCollector) Collect() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.store == nil || m.store.raft == nil {
		return
	}

	r := m.store.raft

	// Update state metric.
	state := r.State()
	raftState.WithLabelValues("Leader").Set(boolToFloat(state == raft.Leader))
	raftState.WithLabelValues("Follower").Set(boolToFloat(state == raft.Follower))
	raftState.WithLabelValues("Candidate").Set(boolToFloat(state == raft.Candidate))
	raftIsLeader.Set(boolToFloat(state == raft.Leader))

	// Get stats from Raft.
	stats := r.Stats()

	// Parse and set numeric metrics.
	if term, err := strconv.ParseFloat(stats["term"], 64); err == nil {
		raftTerm.Set(term)
	}
	if commitIndex, err := strconv.ParseFloat(stats["commit_index"], 64); err == nil {
		raftCommitIndex.Set(commitIndex)
	}
	if lastLogIndex, err := strconv.ParseFloat(stats["last_log_index"], 64); err == nil {
		raftLastLogIndex.Set(lastLogIndex)
	}
	if appliedIndex, err := strconv.ParseFloat(stats["applied_index"], 64); err == nil {
		raftAppliedIndex.Set(appliedIndex)
	}
	if numPeers, err := strconv.ParseFloat(stats["num_peers"], 64); err == nil {
		// num_peers doesn't include self, so add 1.
		raftPeersCount.Set(numPeers + 1)
	}
}

// ObserveApplyLatency records the latency of a Raft apply operation.
func ObserveApplyLatency(d time.Duration) {
	raftApplyLatency.Observe(d.Seconds())
}

// ObserveSnapshotLatency records the latency of a Raft snapshot operation.
func ObserveSnapshotLatency(d time.Duration) {
	raftSnapshotLatency.Observe(d.Seconds())
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
