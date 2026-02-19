//go:build !linux

package vip

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	vipTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_vip_transitions_total",
		Help: "Total number of VIP ownership transitions",
	}, []string{"direction"})
)

func init() {
	vipTransitions.WithLabelValues("acquired")
	vipTransitions.WithLabelValues("lost")
}

// LeadershipSource abstracts leader election for VIP ownership.
type LeadershipSource interface {
	Campaign(ctx context.Context) error
	Resign(ctx context.Context) error
	LeadershipLost() <-chan struct{}
}

// DefaultResignTimeout is the maximum time to wait for VIP resign on shutdown.
const DefaultResignTimeout = 5 * time.Second

// Manager handles virtual IP assignment. On non-Linux platforms, VIP management
// is not supported and the manager operates as a no-op.
type Manager struct {
	vipAddress    string
	ifaceName     string
	renewInterval time.Duration
	resignTimeout time.Duration
	nodeName      string
	logger        *slog.Logger
	leadership    LeadershipSource

	isOwner atomic.Bool
}

// NewManager creates a VIP manager. On non-Linux platforms, VIP assignment
// is not supported and will log a warning.
func NewManager(vipAddress, ifaceName, nodeName string, renewInterval time.Duration, leadership LeadershipSource, logger *slog.Logger) *Manager {
	return &Manager{
		vipAddress:    vipAddress,
		ifaceName:     ifaceName,
		renewInterval: renewInterval,
		resignTimeout: DefaultResignTimeout,
		nodeName:      nodeName,
		logger:        logger.With("component", "vip-manager"),
		leadership:    leadership,
	}
}

// IsOwner returns whether this node currently owns the VIP.
// On non-Linux platforms, this always returns false.
func (m *Manager) IsOwner() bool {
	return m.isOwner.Load()
}

// Run starts the VIP manager. On non-Linux platforms, VIP management is not
// supported and this method logs a warning and waits for context cancellation.
func (m *Manager) Run(ctx context.Context) error {
	if m.vipAddress == "" {
		m.logger.Info("no VIP configured, VIP manager disabled")
		<-ctx.Done()
		return nil
	}

	m.logger.Warn("VIP management is not supported on this platform",
		"platform", "non-linux",
		"vip", m.vipAddress,
	)

	<-ctx.Done()
	return nil
}
