package vip

import (
	"context"

	"github.com/hashicorp/raft"
)

// raftLeadership implements LeadershipSource using Raft leader observation.
type raftLeadership struct {
	raftInstance *raft.Raft
	obsCh        chan raft.Observation
	lostCh       chan struct{}
}

// NewRaftLeadership creates a leadership source backed by Raft.
func NewRaftLeadership(r *raft.Raft) LeadershipSource {
	return &raftLeadership{
		raftInstance: r,
		obsCh:        make(chan raft.Observation, 16),
		lostCh:       make(chan struct{}, 1),
	}
}

func (r *raftLeadership) Campaign(ctx context.Context) error {
	observer := raft.NewObserver(r.obsCh, false, func(o *raft.Observation) bool {
		_, ok := o.Data.(raft.LeaderObservation)
		return ok
	})
	r.raftInstance.RegisterObserver(observer)

	// Wait until we become leader.
	for {
		if r.raftInstance.State() == raft.Leader {
			// Start watching for leadership loss.
			go r.watchLeadership(observer)
			return nil
		}
		select {
		case <-ctx.Done():
			r.raftInstance.DeregisterObserver(observer)
			return ctx.Err()
		case <-r.obsCh:
			// Re-check state on any leadership observation.
		}
	}
}

func (r *raftLeadership) watchLeadership(observer *raft.Observer) {
	for obs := range r.obsCh {
		if lo, ok := obs.Data.(raft.LeaderObservation); ok {
			// If leader address is empty or we're no longer leader, signal loss.
			if lo.LeaderAddr == "" || r.raftInstance.State() != raft.Leader {
				select {
				case r.lostCh <- struct{}{}:
				default:
				}
				r.raftInstance.DeregisterObserver(observer)
				return
			}
		}
	}
}

func (r *raftLeadership) Resign(ctx context.Context) error {
	if r.raftInstance.State() == raft.Leader {
		f := r.raftInstance.LeadershipTransfer()
		return f.Error()
	}
	return nil
}

func (r *raftLeadership) LeadershipLost() <-chan struct{} {
	return r.lostCh
}
