package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"time"

	"github.com/mdenesfe/wax/internal/store"
)

// leaser runs this node's side of the lease protocol, in one goroutine:
//
//   - renew: extend the leases of the instances running here; an instance
//     whose lease could not be renewed is stopped at once (lost);
//   - fence: if the database cannot be reached long enough that the
//     leases might expire, stop everything before another node can take
//     over, so an instance never runs on two nodes;
//   - balance: run a fair share (instances / live nodes, rounded up) by
//     taking free leases and releasing surplus ones;
//   - publish: write the instances' state to their lease rows so any node
//     can answer listings;
//   - claim: between rounds, poll often for leases released by a node that
//     shut down, so a graceful handover takes seconds, not a whole round.
type leaser struct {
	m         *Manager
	ttl       time.Duration
	lastRenew time.Time                   // start of the last successful renewal
	share     int                         // fair share computed by the last round
	published map[string]store.LeaseState // last state written per instance
}

func newLeaser(m *Manager) *leaser {
	return &leaser{m: m, ttl: m.cfg.Timing.LeaseTTL, published: map[string]store.LeaseState{}}
}

// interval between rounds; three rounds fit in one lease lifetime.
func (l *leaser) interval() time.Duration { return l.ttl / 3 }

// claimInterval is how often free leases are polled for between rounds.
func (l *leaser) claimInterval() time.Duration { return min(2*time.Second, l.interval()/3) }

func (l *leaser) loop(ctx context.Context) {
	defer close(l.m.stopped)
	defer l.m.recoverLoop("lease", "")
	t := time.NewTicker(l.interval())
	defer t.Stop()
	claim := time.NewTicker(l.claimInterval())
	defer claim.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.tick(ctx); err != nil && ctx.Err() == nil {
				l.m.log.Warn("lease round failed", "error", err)
			}
		case <-claim.C:
			// One cheap query when something may be free; none at all while
			// this node runs its share.
			if n := l.share - len(l.m.RunningIDs()); n > 0 {
				cctx, cancel := context.WithTimeout(ctx, l.claimInterval())
				if err := l.acquire(cctx, n); err != nil && ctx.Err() == nil {
					l.m.log.Warn("lease claim failed", "error", err)
				}
				cancel()
			}
		case <-l.m.publish:
			l.publishStates(ctx)
		}
	}
}

// tick runs one lease round.
func (l *leaser) tick(ctx context.Context) error {
	m := l.m
	ctx, cancel := context.WithTimeout(ctx, l.interval())
	defer cancel()
	if err := m.store.HeartbeatNode(ctx, m.cfg.NodeID, m.cfg.Address); err != nil {
		l.fenceIfStale()
		return err
	}
	start := time.Now()
	owned, err := m.store.RenewLeases(ctx, m.cfg.NodeID, l.ttl)
	if err != nil {
		l.fenceIfStale()
		return err
	}
	l.lastRenew = start
	l.stopLost(owned)

	nodes, total, err := m.store.ClusterSize(ctx, l.ttl)
	if err != nil {
		return err
	}
	share := fairShare(total, nodes, m.cfg.MaxInstances)
	l.share = share
	running := m.RunningIDs()
	switch {
	case len(running) > share && nodes > 1:
		l.release(ctx, running[share:])
	case len(running) < share:
		if err := l.acquire(ctx, share-len(running)); err != nil {
			return err
		}
	}
	l.publishStates(ctx)
	return nil
}

// fairShare is how many instances one node should run.
func fairShare(instances, nodes, maxInstances int) int {
	share := (instances + max(nodes, 1) - 1) / max(nodes, 1)
	if maxInstances > 0 {
		share = min(share, maxInstances)
	}
	return share
}

// fenceIfStale stops every instance when the last successful renewal is so
// old that the leases may expire before the next one: by then another node
// may take them over. The margin (one round) absorbs clock rate drift and
// the time a stop takes.
func (l *leaser) fenceIfStale() {
	if l.lastRenew.IsZero() || time.Since(l.lastRenew) < l.ttl-l.interval() {
		return
	}
	if lost := l.m.takeRunning(nil); len(lost) > 0 {
		l.m.log.Error("database unreachable; stopping instances before their leases expire", "instances", len(lost))
		l.m.met.lease.Add(float64(len(lost)), "fenced")
		stopAll(lost)
	}
}

// stopLost stops instances running here whose lease was not renewed.
func (l *leaser) stopLost(owned []string) {
	lost := l.m.takeRunning(func(in *Instance) bool { return !slices.Contains(owned, in.ID) })
	for _, in := range lost {
		in.log.Warn("lease lost; stopping instance (another node may run it now)")
		l.m.met.lease.Inc("lost")
	}
	stopAll(lost)
}

// acquire takes up to n free leases and starts those instances.
func (l *leaser) acquire(ctx context.Context, n int) error {
	m := l.m
	ids, err := m.store.AcquireLeases(ctx, m.cfg.NodeID, m.cfg.Address, l.ttl, n, "")
	if err != nil || len(ids) == 0 {
		return err
	}
	cfgs, err := m.store.InstancesByID(ctx, ids)
	if err != nil {
		// Without the configuration the instances cannot start; give the
		// leases back so another node (or the next round) can.
		_ = m.store.ReleaseLeases(context.WithoutCancel(ctx), m.cfg.NodeID, ids)
		return err
	}
	for _, cfg := range cfgs {
		if _, err := m.run(cfg); err != nil {
			return err
		}
		m.log.Info("instance acquired", "instance", cfg.ID)
		m.met.lease.Inc("acquired")
	}
	return nil
}

// release stops the given instances and gives their leases back, for
// another node to take over.
func (l *leaser) release(ctx context.Context, ids []string) {
	m := l.m
	list := m.takeRunning(func(in *Instance) bool { return slices.Contains(ids, in.ID) })
	stopAll(list)
	if err := m.store.ReleaseLeases(ctx, m.cfg.NodeID, ids); err != nil {
		m.log.Warn("could not release leases; they expire on their own", "error", err)
	}
	m.log.Info("instances released for rebalancing", "count", len(list))
	m.met.lease.Add(float64(len(list)), "released")
}

// publishStates writes changed instance states to their lease rows.
func (l *leaser) publishStates(ctx context.Context) {
	m := l.m
	changed := map[string]store.LeaseState{}
	m.mu.Lock()
	for id, in := range m.instances {
		st := store.LeaseState{Status: string(in.Status()), QRPending: in.QR() != ""}
		if l.published[id] != st {
			changed[id] = st
		}
	}
	m.mu.Unlock()
	if err := m.store.PublishLeaseStates(ctx, m.cfg.NodeID, changed); err != nil {
		if ctx.Err() == nil {
			m.log.Warn("could not publish instance states", "error", err)
		}
		return
	}
	for id, st := range changed {
		l.published[id] = st
	}
}

// RunningIDs returns the internal ids of the instances this node runs,
// sorted (when rebalancing, the last ones are released first).
func (m *Manager) RunningIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// takeRunning removes and returns the running instances matching keep
// (all of them if keep is nil); the caller stops them.
func (m *Manager) takeRunning(match func(*Instance) bool) []*Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Instance
	for id, in := range m.instances {
		if match == nil || match(in) {
			out = append(out, in)
			delete(m.instances, id)
		}
	}
	return out
}

// randomNodeID names a node that was not given an id: hostname plus a
// random suffix, so restarts and replicas never collide.
func randomNodeID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "node"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
}
