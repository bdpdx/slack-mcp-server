package agentchat

import (
	"context"

	"go.uber.org/zap"
)

// Pinner is implemented by a Deliverer that pins a subscription to its
// agent process and seat before it is registered (CS-42). An error refuses
// the watch; a warning is passed back to the agent.
type Pinner interface {
	Pin(ctx context.Context, sub *Subscription) (warning string, err error)
}

// replaceSeat drops the other sessions in sub's seat: the same Claude
// process (one inbox socket, which a /clear keeps under a new session ID),
// or the same Codex working directory (a restarted agent's new thread,
// while the app-server still holds the old one).
func (l *Listener) replaceSeat(sub *Subscription) {
	l.mu.Lock()
	var gone []string
	for id, other := range l.state.Subscriptions {
		if id == sub.SessionID || other.Kind != sub.Kind {
			continue
		}
		if (sub.Kind == KindClaude && sub.Socket != "" && other.Socket == sub.Socket) ||
			(sub.Kind == KindCodex && sub.Seat != "" && other.Seat == sub.Seat) {
			gone = append(gone, id)
		}
	}
	l.mu.Unlock()
	for _, id := range gone {
		l.Log.Info("replaced by a newer session in the same seat; dropping its subscription", zap.String("session", id), zap.String("by", sub.SessionID))
		l.Unsubscribe(id, "")
	}
}

// watchPin starts watching sub's agent process, once per process, so its
// watch ends the moment the agent exits.
func (l *Listener) watchPin(sub *Subscription) {
	if l.ExitWatch == nil || sub.PinPID <= 0 {
		return
	}
	key := [2]int64{int64(sub.PinPID), sub.PinStart}
	l.mu.Lock()
	if l.pinWatched == nil {
		l.pinWatched = map[[2]int64]bool{}
	}
	if l.pinWatched[key] {
		l.mu.Unlock()
		return
	}
	l.pinWatched[key] = true
	l.mu.Unlock()
	pid, start := sub.PinPID, sub.PinStart
	if !l.ExitWatch(pid, start, func() { l.pinExited(pid, start) }) {
		l.mu.Lock()
		delete(l.pinWatched, key)
		l.mu.Unlock()
	}
}

// WatchPins watches the agent processes of every restored subscription.
func (l *Listener) WatchPins() {
	l.mu.Lock()
	subs := make([]*Subscription, 0, len(l.state.Subscriptions))
	for _, sub := range l.state.Subscriptions {
		subs = append(subs, snapshot(sub))
	}
	l.mu.Unlock()
	for _, sub := range subs {
		l.watchPin(sub)
	}
}

// pinExited drops every subscription pinned to the process that exited.
func (l *Listener) pinExited(pid int, start int64) {
	l.mu.Lock()
	delete(l.pinWatched, [2]int64{int64(pid), start})
	var gone []string
	for id, sub := range l.state.Subscriptions {
		if sub.PinPID == pid && sub.PinStart == start {
			gone = append(gone, id)
		}
	}
	l.mu.Unlock()
	for _, id := range gone {
		l.Log.Info("agent process exited; dropping its subscription", zap.String("session", id), zap.Int("pid", pid))
		l.Unsubscribe(id, "")
	}
}
