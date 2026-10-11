package agentchat

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sessions(l *Listener) []string {
	var ids []string
	for _, s := range l.Status() {
		ids = append(ids, s.SessionID)
	}
	return ids
}

// CS-42: a new watch from the same Claude process (one inbox socket; e.g.
// after /clear starts a new session ID) replaces the old one, so the
// process is not delivered everything twice.
func TestSameClaudeProcessReplaces(t *testing.T) {
	ctx := context.Background()
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	old, cur := claudeSub("s1"), claudeSub("s2")
	old.Socket, cur.Socket = "/tmp/cc-socks/42.sock", "/tmp/cc-socks/42.sock"
	require.NoError(t, l.Subscribe(ctx, old, 0))
	require.NoError(t, l.Subscribe(ctx, claudeSub("other"), 0)) // another Claude process
	require.NoError(t, l.Subscribe(ctx, cur, 0))
	assert.Equal(t, []string{"other", "s2"}, sessions(l))
}

// CS-42: a new Codex thread in the same seat (home and working directory)
// takes over from the old thread, which the app-server may keep loaded for
// minutes after its terminal closed.
func TestSameCodexSeatReplaces(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	sub := func(id, seat string) *Subscription {
		return &Subscription{SessionID: id, Kind: KindCodex, ThreadID: id, Seat: seat, Channels: []string{"C1"}}
	}
	require.NoError(t, l.Subscribe(ctx, sub("t1", "/code/r1"), 0))
	require.NoError(t, l.Subscribe(ctx, sub("t2", "/code/b1"), 0))
	require.NoError(t, l.Subscribe(ctx, sub("t3", "/code/r1"), 0))
	assert.Equal(t, []string{"t2", "t3"}, sessions(l))
	require.NoError(t, l.Subscribe(ctx, sub("t4", ""), 0)) // seat unknown: replaces nothing
	assert.Equal(t, []string{"t2", "t3", "t4"}, sessions(l))
}

// CS-42: the watch ends the moment its agent process exits.
func TestAgentExitDropsWatch(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	var onExit func()
	l.ExitWatch = func(pid int, start int64, f func()) bool {
		assert.Equal(t, 4242, pid)
		assert.Equal(t, int64(7), start)
		onExit = f
		return true
	}
	sub := claudeSub("s1")
	sub.PinPID, sub.PinStart = 4242, 7
	require.NoError(t, l.Subscribe(ctx, sub, 0))
	require.NoError(t, l.Subscribe(ctx, claudeSub("s2"), 0))
	require.NotNil(t, onExit)
	onExit()
	assert.Equal(t, []string{"s2"}, sessions(l))

	// A listener that restarts watches the pins it restored.
	var rewatched []int
	l2 := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	l2.state = l.state
	l2.state.Subscriptions["s3"] = &Subscription{SessionID: "s3", Kind: KindClaude, Socket: "/s/s3", Token: "t", Channels: []string{"C1"}, PinPID: 99, PinStart: 1}
	l2.ExitWatch = func(pid int, _ int64, _ func()) bool { rewatched = append(rewatched, pid); return true }
	l2.WatchPins()
	assert.Equal(t, []int{99}, rewatched)
}

type pinDeliverer struct {
	fakeDeliverer
	pin func(*Subscription) (string, error)
}

func (p *pinDeliverer) Pin(_ context.Context, sub *Subscription) (string, error) { return p.pin(sub) }

// The deliverer pins a subscription before it is registered; a refusal
// registers nothing, and a warning is passed back to the agent.
func TestSubscribePins(t *testing.T) {
	ctx := context.Background()
	d := &pinDeliverer{pin: func(sub *Subscription) (string, error) {
		if sub.SessionID == "child" {
			return "", errors.New("sub-agent thread")
		}
		sub.PinPID = 77
		return "not pinned: no terminal found", nil
	}}
	l := newTestListener(t, newFakeSlack(), &d.fakeDeliverer)
	l.Deliverer = d
	l.ExitWatch = func(int, int64, func()) bool { return true }
	resp := l.Control(ctx, ControlRequest{Op: "subscribe", Subscription: claudeSub("child")})
	assert.Contains(t, resp.Error, "sub-agent thread")
	assert.Empty(t, sessions(l))
	resp = l.Control(ctx, ControlRequest{Op: "subscribe", Subscription: claudeSub("s1")})
	assert.True(t, resp.OK)
	assert.Equal(t, "not pinned: no terminal found", resp.Warning)
	l.mu.Lock()
	assert.Equal(t, 77, l.state.Subscriptions["s1"].PinPID)
	l.mu.Unlock()
}

func TestClaudePinFromSocket(t *testing.T) {
	assert.Equal(t, 25727, pidFromSocket("/tmp/cc-socks/25727.sock"))
	assert.Equal(t, 0, pidFromSocket("/tmp/other/claude.sock"))
	assert.Equal(t, 0, pidFromSocket(""))
}

// The Codex terminal driving a thread: the newest `codex --remote` whose
// working directory (-C/--cd) is the thread's.
func TestFindCodexTUI(t *testing.T) {
	ps := []procRow{
		{PID: 10, Start: 100, Command: "codex --remote unix:// -C /code/r1"},
		{PID: 11, Start: 300, Command: "/opt/homebrew/bin/codex --remote unix:// -C /code/r1"},
		{PID: 12, Start: 400, Command: "codex --remote unix:// --cd /code/b1"},
		{PID: 13, Start: 500, Command: "/x/codex app-server --listen unix://"},
		{PID: 14, Start: 600, Command: "codex --remote unix:// -C /code/r1-other"},
	}
	assert.Equal(t, procRow{PID: 11, Start: 300, Command: ps[1].Command}, mustFind(t, ps, "/code/r1"))
	assert.Equal(t, 12, mustFind(t, ps, "/code/b1").PID)
	_, ok := findCodexTUI(ps, "/code/none")
	assert.False(t, ok)
}

func mustFind(t *testing.T, ps []procRow, cwd string) procRow {
	t.Helper()
	r, ok := findCodexTUI(ps, cwd)
	require.True(t, ok)
	return r
}

// HostDeliverer treats a session whose pinned agent exited as gone, before
// any delivery: a Codex thread outlives its terminal.
func TestPinnedSessionGoneWhenAgentExits(t *testing.T) {
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	d := &HostDeliverer{}
	sub := &Subscription{SessionID: "t1", Kind: KindCodex, ThreadID: "t1", PinPID: cmd.Process.Pid, PinStart: 1}
	alive, err := d.Alive(context.Background(), sub)
	require.NoError(t, err)
	assert.False(t, alive)
	_, err = d.Deliver(context.Background(), sub, "id", "text")
	assert.ErrorIs(t, err, ErrSessionGone)
}
