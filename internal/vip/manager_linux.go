//go:build linux

package vip

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/vishvananda/netlink"
)

var (
	vipTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pgbastion_vip_transitions_total",
		Help: "Total number of VIP ownership transitions",
	}, []string{"direction"})
)

func init() {
	// Pre-initialize metrics with all label combinations so they appear in /metrics.
	vipTransitions.WithLabelValues("acquired")
	vipTransitions.WithLabelValues("lost")
}

// LeadershipSource abstracts leader election for VIP ownership.
type LeadershipSource interface {
	// Campaign blocks until this node becomes the leader, or ctx is cancelled.
	Campaign(ctx context.Context) error
	// Resign gives up leadership.
	Resign(ctx context.Context) error
	// LeadershipLost returns a channel that is signaled when leadership is lost.
	LeadershipLost() <-chan struct{}
}

// DefaultResignTimeout is the maximum time to wait for VIP resign on shutdown.
const DefaultResignTimeout = 5 * time.Second

// Manager handles virtual IP assignment using a Raft-based leadership source.
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

// NewManager creates a VIP manager with the given leadership source.
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
func (m *Manager) IsOwner() bool {
	return m.isOwner.Load()
}

// Run starts the VIP lease competition loop. Blocks until context is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	if m.vipAddress == "" {
		m.logger.Info("no VIP configured, VIP manager disabled")
		<-ctx.Done()
		return nil
	}

	if m.leadership == nil {
		m.logger.Warn("no leadership source configured, VIP manager disabled")
		<-ctx.Done()
		return nil
	}

	m.logger.Info("starting VIP manager",
		"vip", m.vipAddress,
		"interface", m.ifaceName,
	)

	for {
		err := m.campaignForVIP(ctx)
		if ctx.Err() != nil {
			m.removeVIP()
			return ctx.Err()
		}
		if err != nil {
			m.logger.Error("VIP campaign failed, retrying", "error", err)
			m.removeVIP()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.renewInterval):
			}
		}
	}
}

func (m *Manager) campaignForVIP(ctx context.Context) error {
	m.logger.Info("campaigning for VIP leadership")
	if err := m.leadership.Campaign(ctx); err != nil {
		return fmt.Errorf("campaign failed: %w", err)
	}

	m.logger.Info("won VIP leadership, assigning VIP", "vip", m.vipAddress)
	if err := m.assignVIP(); err != nil {
		m.logger.Error("failed to assign VIP", "error", err)
		_ = m.leadership.Resign(ctx)
		return err
	}
	m.isOwner.Store(true)

	// Hold leadership until context is cancelled or leadership is lost.
	select {
	case <-ctx.Done():
		m.logger.Info("context cancelled, resigning VIP leadership")
		m.removeVIP()
		// Use bounded timeout for resign to avoid hanging on network issues.
		resignCtx, cancel := context.WithTimeout(context.Background(), m.resignTimeout)
		if err := m.leadership.Resign(resignCtx); err != nil {
			m.logger.Warn("failed to resign leadership", "error", err, "timeout", m.resignTimeout)
		}
		cancel()
		return ctx.Err()
	case <-m.leadership.LeadershipLost():
		m.logger.Warn("leadership lost, removing VIP")
		m.removeVIP()
		return fmt.Errorf("leadership lost")
	}
}

func (m *Manager) assignVIP() error {
	link, err := netlink.LinkByName(m.ifaceName)
	if err != nil {
		return fmt.Errorf("finding interface %s: %w", m.ifaceName, err)
	}

	addr, err := netlink.ParseAddr(m.vipAddress + "/32")
	if err != nil {
		return fmt.Errorf("parsing VIP address: %w", err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		// Ignore "file exists" error if the address is already assigned.
		if err.Error() != "file exists" {
			return fmt.Errorf("adding VIP to interface: %w", err)
		}
	}

	m.logger.Info("VIP assigned", "vip", m.vipAddress, "interface", m.ifaceName)
	vipTransitions.WithLabelValues("acquired").Inc()

	// Send gratuitous ARP.
	if err := SendGratuitousARP(m.ifaceName, net.ParseIP(m.vipAddress)); err != nil {
		m.logger.Warn("failed to send gratuitous ARP", "error", err)
	}

	return nil
}

func (m *Manager) removeVIP() {
	if !m.isOwner.Load() {
		return
	}

	link, err := netlink.LinkByName(m.ifaceName)
	if err != nil {
		m.logger.Error("failed to find interface for VIP removal", "interface", m.ifaceName, "error", err)
		return
	}

	addr, err := netlink.ParseAddr(m.vipAddress + "/32")
	if err != nil {
		m.logger.Error("failed to parse VIP address for removal", "error", err)
		return
	}

	if err := netlink.AddrDel(link, addr); err != nil {
		m.logger.Error("failed to remove VIP from interface", "error", err)
		return
	}

	m.isOwner.Store(false)
	vipTransitions.WithLabelValues("lost").Inc()
	m.logger.Info("VIP removed", "vip", m.vipAddress, "interface", m.ifaceName)
}
