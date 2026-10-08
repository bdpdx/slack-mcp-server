package agentchat

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// shutdown replies first, then stops the daemon; a listener not run as a
// daemon refuses.
func TestShutdownOp(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	resp := l.Control(ctx, ControlRequest{Op: "shutdown"})
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Error)

	stopped := make(chan struct{})
	l.Stop = func() { close(stopped) }
	resp = l.Control(ctx, ControlRequest{Op: "shutdown"})
	require.True(t, resp.OK)
	assert.NotEmpty(t, resp.Version)
	select {
	case <-stopped:
		t.Fatal("stopped before the reply could be written")
	default:
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("never stopped")
	}
}

// A fake home for replaceWith: running is the listener's build ("" when
// none runs); start brings up this binary's build unless a racing watch
// command started an older one first.
type fakeHome struct {
	running       string
	racers        []string // builds that win the start gap, one per start
	stops, starts int
}

func (h *fakeHome) probe() (string, bool) { return h.running, h.running != "" }
func (h *fakeHome) stop() error           { h.stops++; h.running = ""; return nil }
func (h *fakeHome) start() error {
	h.starts++
	if h.running != "" {
		return nil // ensureListener accepts any answering listener
	}
	if len(h.racers) > 0 {
		h.running, h.racers = h.racers[0], h.racers[1:]
		return nil
	}
	h.running = "new"
	return nil
}

// Restart succeeds only when the home ends up on this binary's build: an
// older listener that wins the gap is replaced again, and one that keeps
// winning, or a listener that dies, is an error.
func TestReplaceWithVerifiesTheBuild(t *testing.T) {
	h := &fakeHome{running: "old"}
	v, err := replaceWith(h.probe, h.stop, h.start, "new", "log", nil)
	require.NoError(t, err)
	assert.Equal(t, "new", v)
	assert.Equal(t, 1, h.stops)

	h = &fakeHome{running: "old", racers: []string{"old"}}
	v, err = replaceWith(h.probe, h.stop, h.start, "new", "log", nil)
	require.NoError(t, err, "an older listener that won the gap is replaced again")
	assert.Equal(t, "new", v)
	assert.Equal(t, 2, h.stops)

	h = &fakeHome{running: "old", racers: []string{"old", "old", "old"}}
	_, err = replaceWith(h.probe, h.stop, h.start, "new", "log", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "old, not this binary's new")

	dies := &fakeHome{running: "old"}
	_, err = replaceWith(dies.probe, dies.stop, func() error { dies.starts++; return nil }, "new", "log", nil)
	require.Error(t, err, "a listener that exits right after starting is not success")
	assert.Contains(t, err.Error(), "exited right after starting")
}

// testHome is a home in a short temporary directory (unix socket paths are
// limited to about 100 bytes).
func testHome(t *testing.T) Home {
	dir, err := os.MkdirTemp("/tmp", "smcp")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := NewHome(filepath.Join(dir, EnvFileName))
	require.NoError(t, os.MkdirAll(h.StateDir, 0o700))
	return h
}

// fakeListener serves h's control socket with handler and holds its lock,
// as a running listener does; stop ends both.
func fakeListener(t *testing.T, h Home, handler ControlHandler) (stop func()) {
	release, err := AcquireListenerLock(filepath.Join(h.StateDir, "listener.lock"))
	require.NoError(t, err)
	ln, err := ListenControl(h.ControlSocket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go ServeControl(ctx, ln, handler)
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			ln.Close()
			os.Remove(h.ControlSocket)
			release()
		})
	}
	t.Cleanup(stop)
	return stop
}

// A listener that knows the shutdown op is asked to stop and waited for.
func TestStopListenerByOp(t *testing.T) {
	h := testHome(t)
	var stop func()
	stop = fakeListener(t, h, func(ctx context.Context, req ControlRequest) ControlResponse {
		if req.Op == "shutdown" {
			time.AfterFunc(50*time.Millisecond, stop)
		}
		return ControlResponse{OK: true, Version: "v1"}
	})
	c := &cli{home: h, kill: func() error { t.Fatal("no SIGTERM for a listener that knows the op"); return nil }}
	v, up := c.listenerState(context.Background())
	require.True(t, up)
	assert.Equal(t, "v1", v)
	require.NoError(t, c.stopListener(context.Background()))
	_, up = c.listenerState(context.Background())
	assert.False(t, up)
}

// A listener too old for the op is stopped by SIGTERM.
func TestStopOldListenerBySignal(t *testing.T) {
	h := testHome(t)
	stop := fakeListener(t, h, func(ctx context.Context, req ControlRequest) ControlResponse {
		if req.Op == "shutdown" {
			return ControlResponse{Error: "unknown op shutdown"}
		}
		return ControlResponse{OK: true}
	})
	killed := 0
	c := &cli{home: h, kill: func() error { killed++; stop(); return nil }}
	v, up := c.listenerState(context.Background())
	require.True(t, up)
	assert.Equal(t, "unknown", v, "a listener too old to report its build")
	require.NoError(t, c.stopListener(context.Background()))
	assert.Equal(t, 1, killed)
}

// A listener that accepts connections but never answers is running, not
// gone, and is stopped by SIGTERM.
func TestUnresponsiveListener(t *testing.T) {
	defer func(p, s time.Duration) { probeTimeout, shutdownTimeout = p, s }(probeTimeout, shutdownTimeout)
	probeTimeout, shutdownTimeout = 100*time.Millisecond, 100*time.Millisecond
	h := testHome(t)
	block := make(chan struct{})
	defer close(block)
	stop := fakeListener(t, h, func(ctx context.Context, req ControlRequest) ControlResponse {
		<-block
		return ControlResponse{OK: true}
	})
	killed := 0
	c := &cli{home: h, kill: func() error { killed++; stop(); return nil }}
	v, up := c.listenerState(context.Background())
	assert.True(t, up, "a timeout is not proof that nothing runs")
	assert.Equal(t, "unresponsive", v)
	require.NoError(t, c.stopListener(context.Background()))
	assert.Equal(t, 1, killed)
}

// No socket at all means no listener.
func TestNoListener(t *testing.T) {
	c := &cli{home: testHome(t)}
	_, up := c.listenerState(context.Background())
	assert.False(t, up)
}

// A start that fails is tried again before restart gives up, and the error
// names the recovery command.
func TestReplaceWithRetriesStart(t *testing.T) {
	h := &fakeHome{running: "old"}
	fails := 1
	start := func() error {
		if fails > 0 {
			fails--
			return errors.New("auth.test failed")
		}
		return h.start()
	}
	v, err := replaceWith(h.probe, h.stop, start, "new", "RECOVER", nil)
	require.NoError(t, err)
	assert.Equal(t, "new", v)

	h = &fakeHome{running: "old"}
	_, err = replaceWith(h.probe, h.stop, func() error { return errors.New("auth.test failed") }, "new", "RECOVER", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not start")
	assert.Contains(t, err.Error(), "RECOVER")
}

// A listener that accepts the shutdown but never exits is signalled after
// the wait; one that exits just before the signal is not a failure.
func TestAcceptedShutdownButStillRunning(t *testing.T) {
	defer func(w time.Duration) { stopWait = w }(stopWait)
	stopWait = 300 * time.Millisecond
	h := testHome(t)
	stop := fakeListener(t, h, func(ctx context.Context, req ControlRequest) ControlResponse {
		return ControlResponse{OK: true, Version: "v1"}
	})
	killed := 0
	c := &cli{home: h, kill: func() error { killed++; stop(); return nil }}
	require.NoError(t, c.stopListener(context.Background()))
	assert.Equal(t, 1, killed)

	h = testHome(t)
	stop = fakeListener(t, h, func(ctx context.Context, req ControlRequest) ControlResponse {
		if req.Op == "shutdown" {
			time.AfterFunc(stopWait+50*time.Millisecond, stop) // exits right at the boundary
		}
		return ControlResponse{OK: true, Version: "v1"}
	})
	c = &cli{home: h, kill: func() error { return errors.New("no listener process found") }}
	require.NoError(t, c.stopListener(context.Background()))
}

// A stale socket file with nothing listening reads as no listener.
func TestStaleSocketIsNoListener(t *testing.T) {
	h := testHome(t)
	ln, err := net.Listen("unix", h.ControlSocket)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	_, statErr := os.Stat(h.ControlSocket)
	require.NoError(t, statErr, "the socket file stays behind")
	c := &cli{home: h}
	_, up := c.listenerState(context.Background())
	assert.False(t, up)
	assert.True(t, c.waitStopped(context.Background()))
}

// Shutdown waits for hooks to collect answers the owner already gave, and
// refuses, leaving the listener running and recording, if they stay
// uncollected.
func TestShutdownDrainsOrRefuses(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "a", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleInteraction(clickOn("UBR", decisionDeny, "a", "C1", "2000.1", "BCL"))
	stopped := make(chan struct{})
	l.Stop = func() { close(stopped) }
	done := make(chan ControlResponse, 1)
	go func() { done <- l.Control(ctx, ControlRequest{Op: "shutdown"}) }()
	select {
	case <-done:
		t.Fatal("shutdown replied while an accepted denial was uncollected")
	case <-time.After(300 * time.Millisecond):
	}
	d, _ := takeApproval(t, l, "a")
	assert.Equal(t, decisionDeny, d)
	assert.True(t, (<-done).OK)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("never stopped")
	}

	defer func(d time.Duration) { answerDrain = d }(answerDrain)
	answerDrain = 200 * time.Millisecond
	l = newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	early := make(chan struct{}, 1)
	l.Stop = func() { early <- struct{}{} }
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "b", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleInteraction(clickOn("UBR", decisionDeny, "b", "C1", "2000.1", "BCL"))
	resp := l.Control(ctx, ControlRequest{Op: "shutdown"})
	assert.False(t, resp.OK, "refused: the restart fails instead of losing the answer")
	assert.Contains(t, resp.Error, "answers are waiting")
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "c", Channel: "C1", TS: "2000.2"}).OK)
	l.HandleInteraction(clickOn("UBR", decisionDeny, "c", "C1", "2000.2", "BCL"))
	d, _ = takeApproval(t, l, "c")
	assert.Equal(t, decisionDeny, d, "a refused shutdown leaves the listener recording")
	d, _ = takeApproval(t, l, "b")
	assert.Equal(t, decisionDeny, d, "and the answer it protected is still there")
	select {
	case <-early:
		t.Fatal("stopped with an accepted answer still uncollected")
	case <-time.After(shutdownDelay + 100*time.Millisecond):
	}
}

// Once fenced, no answer is recorded. A click gets a click-again note; an
// owner's reply to a waiting request, even one already past HandleMessage's
// entry check, is kept out of the session (consumed) and gets an
// answer-again note; any other message is left unmarked for the next
// listener's recovery.
func TestShutdownFenceRecordsNothing(t *testing.T) {
	ctx := context.Background()
	api, del := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, del)
	require.NoError(t, l.Subscribe(ctx, claudeSub("s1"), 0))
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "a", Channel: "C1", TS: "2000.1"}).OK)
	l.Stop = func() {}
	require.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)

	l.HandleInteraction(clickOn("UBR", decisionDeny, "a", "C1", "2000.1", "BCL"))
	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.2", ThreadTS: "2000.1", User: "UBR", Text: "no, wrong table"})
	inflight := Message{Channel: "C1", TS: "2000.4", User: "UBR", Text: "no, use staging"}
	assert.True(t, l.approvalReply(inflight), "a reply already past the entry check is consumed too")
	l.markConsumed(inflight)
	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.3", User: "UBR", Text: "hello"})
	d, _ := takeApproval(t, l, "a")
	assert.Equal(t, "", d, "neither the click nor the replies were recorded")
	assert.True(t, l.state.WasDelivered("s1", "C1", "2000.2"), "consumed, so recovery never pushes it to the session")
	assert.True(t, l.state.WasDelivered("s1", "C1", "2000.4"))
	assert.False(t, l.state.WasDelivered("s1", "C1", "2000.3"), "an ordinary message is left for recovery")
	assert.Empty(t, del.got)
	l.WaitNotes(2 * time.Second)
	var clickNote, replyNote int
	for _, p := range api.posts() {
		if strings.Contains(p, "Click again") {
			clickNote++
		}
		if strings.Contains(p, "Answer again") {
			replyNote++
		}
	}
	assert.Equal(t, 1, clickNote)
	assert.Equal(t, 2, replyNote)
}

// A click on a request no hook registered (stale buttons) can never be
// collected, so it does not hold up a restart.
func TestOrphanAnswerDoesNotBlockShutdown(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	l.HandleInteraction(clickOn("UBR", decisionDeny, "ghost", "C1", "2000.1", "BCL"))
	l.HandleInteraction(clickOn("UBR", decisionAllow, "ghost2", "C1", "2000.2", "BCL"))
	now = now.Add(copyRegisterWait)
	l.Stop = func() {}
	assert.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
}

// A denial clicked between a hook posting its copy and registering it holds
// the shutdown until the copy registers and the hook collects it.
func TestFreshUnregisteredAnswerHoldsShutdown(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	l.HandleInteraction(clickOn("UBR", decisionDeny, "f", "C1", "2000.1", "BCL"))
	l.Stop = func() {}
	done := make(chan ControlResponse, 1)
	go func() { done <- l.Control(ctx, ControlRequest{Op: "shutdown"}) }()
	select {
	case <-done:
		t.Fatal("fenced while a fresh denial waited for its copy to register")
	case <-time.After(300 * time.Millisecond):
	}
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "f", Channel: "C1", TS: "2000.1"}).OK)
	d, _ := takeApproval(t, l, "f")
	assert.Equal(t, decisionDeny, d)
	assert.True(t, (<-done).OK)
}

// A copy registered after the fence does not apply a waiting click (it
// would be lost); the click is dropped and the owner asked to click again.
func TestRegistrationAfterFenceAppliesNoClick(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	l := newTestListener(t, api, &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	l.HandleInteraction(clickOn("UBR", decisionAllow, "g", "C1", "2000.1", "BCL"))
	now = now.Add(copyRegisterWait)
	l.Stop = func() {}
	require.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "g", Channel: "C1", TS: "2000.1"}).OK)
	d, _ := takeApproval(t, l, "g")
	assert.Equal(t, "", d)
	l.WaitNotes(2 * time.Second)
	found := false
	for _, p := range api.posts() {
		found = found || strings.Contains(p, "Click again")
	}
	assert.True(t, found)
}

// A hook that ends during shutdown with its redraw failed gets the redraw
// done before the listener exits, not left to a sweep that never runs.
func TestOwedRedrawDuringShutdown(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	l := newTestListener(t, api, &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "h", Channel: "C1", TS: "2000.1", Text: "req"}).OK)
	takeApproval(t, l, "h")
	l.Stop = func() {}
	require.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-end", Approval: "h", Text: "✅ Allowed."}).OK)
	l.WaitNotes(2 * time.Second)
	assert.Equal(t, []string{"C1|2000.1|✅ Allowed."}, api.updates())
}

// WaitNotes waits for work started while it waits, too.
func TestWaitNotesCoversLateWork(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	finished := make(chan struct{})
	l.mu.Lock()
	l.track(func() {
		time.Sleep(50 * time.Millisecond)
		l.mu.Lock()
		l.track(func() { time.Sleep(100 * time.Millisecond); close(finished) })
		l.mu.Unlock()
	})
	l.mu.Unlock()
	l.WaitNotes(2 * time.Second)
	select {
	case <-finished:
	default:
		t.Fatal("returned before late work finished")
	}
}

// A shutdown refused because answers are waiting fails the restart: the old
// listener is neither signalled nor replaced.
func TestRefusedShutdownFailsRestart(t *testing.T) {
	h := testHome(t)
	fakeListener(t, h, func(ctx context.Context, req ControlRequest) ControlResponse {
		if req.Op == "shutdown" {
			return ControlResponse{Error: "answers are waiting for their hooks; try the restart again shortly"}
		}
		return ControlResponse{OK: true, Version: "v1"}
	})
	killed := 0
	c := &cli{home: h, kill: func() error { killed++; return nil }}
	err := c.stopListener(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "answers are waiting")
	assert.Equal(t, 0, killed)
	started := 0
	_, err = replaceWith(func() (string, bool) { return "v1", true }, func() error { return c.stopListener(context.Background()) },
		func() error { started++; return nil }, "v2", "RECOVER", nil)
	require.Error(t, err)
	assert.Equal(t, 0, started, "no start after a refused stop")
}

// The hold for an unregistered copy runs from the latest click, so a denial
// clicked after an older click on the same request still holds the drain.
func TestLatestClickRestartsCopyWait(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	l.HandleInteraction(clickOn("UBR", decisionAllow, "k", "C1", "2000.1", "BCL"))
	now = now.Add(copyRegisterWait + time.Second)
	l.HandleInteraction(clickOn("UBR", decisionDeny, "k", "C1", "2000.1", "BCL"))
	l.mu.Lock()
	waiting := l.answersWaitingLocked()
	l.mu.Unlock()
	assert.True(t, waiting, "the fresh denial holds the drain")
}

// A denial clicked on a copy before it registered, whose registration lands
// after the fence, is cleared with a click-again note rather than kept for
// a listener about to exit.
func TestLateRegistrationClearsUncollectedDecision(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	l := newTestListener(t, api, &fakeDeliverer{})
	now := time.Unix(2000, 0)
	l.Now = func() time.Time { return now }
	l.HandleInteraction(clickOn("UBR", decisionDeny, "m", "C2", "3000.1", "BCL"))
	now = now.Add(copyRegisterWait)
	l.Stop = func() {}
	require.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "m", Channel: "C1", TS: "2000.1"}).OK)
	d, _ := takeApproval(t, l, "m")
	assert.Equal(t, "", d)
	l.WaitNotes(2 * time.Second)
	found := false
	for _, p := range api.posts() {
		found = found || (strings.HasPrefix(p, "C2|") && strings.Contains(p, "Click again"))
	}
	assert.True(t, found, "the note goes to the copy where the denial was clicked")
}

// heldDeliverer holds each delivery until release is closed.
type heldDeliverer struct {
	fakeDeliverer
	entered chan struct{} // receives once per delivery that has started
	release chan struct{}
}

func (d *heldDeliverer) Deliver(ctx context.Context, sub *Subscription, clientID, text string) (string, error) {
	d.entered <- struct{}{}
	<-d.release
	return d.fakeDeliverer.Deliver(ctx, sub, clientID, text)
}

// A shutdown lets a delivery in progress finish and be marked before the
// listener stops, so the next listener never delivers it a second time.
func TestShutdownWaitsForDeliveries(t *testing.T) {
	ctx := context.Background()
	d := &heldDeliverer{entered: make(chan struct{}, 4), release: make(chan struct{})}
	l, err := NewListener(newFakeSlack(), d, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	l.Async = true
	require.NoError(t, l.Subscribe(ctx, claudeSub("s1"), 0))
	l.HandleMessage(ctx, Message{Channel: "C1", TS: "2000.5", User: "UBR", Text: "hello"})
	<-d.entered // the delivery is under way
	stopped := make(chan struct{})
	l.Stop = func() { close(stopped) }
	require.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	select {
	case <-stopped:
		t.Fatal("stopped with a delivery in progress")
	case <-time.After(shutdownDelay + 300*time.Millisecond):
	}
	close(d.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("never stopped")
	}
	l.mu.Lock()
	marked := l.state.WasDelivered("s1", "C1", "2000.5")
	l.mu.Unlock()
	assert.True(t, marked, "delivered and marked before the stop")
}

// Messages kept for GM re-classification survive a listener restart.
func TestRechecksSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	l, err := NewListener(newFakeSlack(), &fakeDeliverer{}, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	l.rememberRecheck(Message{Channel: "C1", TS: "2000.1", User: "UBR", Text: "<@UGM> are you there?"})
	l2, err := NewListener(newFakeSlack(), &fakeDeliverer{}, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	require.Len(t, l2.state.Rechecks, 1)
	assert.Equal(t, "<@UGM> are you there?", l2.state.Rechecks[watchKey("C1", "2000.1")].M.Text)
}

// A delivery not yet started when the restart begins is not started at all:
// it stays unmarked for the next listener, which delivers it once.
func TestShutdownStartsNoNewDelivery(t *testing.T) {
	ctx := context.Background()
	d := &heldDeliverer{entered: make(chan struct{}, 4), release: make(chan struct{})}
	close(d.release)
	l, err := NewListener(newFakeSlack(), d, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, l.Subscribe(ctx, claudeSub("s1"), 0))
	l.Stop = func() {}
	require.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	l.deliverTo(ctx, l.state.Subscriptions["s1"], []pending{{msg: Message{Channel: "C1", TS: "2000.6", User: "UBR", Text: "late"}}})
	assert.Empty(t, d.got, "nothing delivered after the fence")
	assert.False(t, l.state.WasDelivered("s1", "C1", "2000.6"), "left for the next listener")
	assert.Error(t, l.deliverCohort(ctx, l.state.Subscriptions["s1"], "k", "notice"), "cohort notices wait for the next listener too")
}

// A watch started after the fence gets its backlog from the next listener,
// exactly once.
func TestBacklogAfterFenceGoesToNextListener(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	api.history["C1"] = []slack.Message{msg("1999.5", "UBR", "context before the watch")}
	path := filepath.Join(t.TempDir(), "state.json")
	d1 := &fakeDeliverer{}
	l1, err := NewListener(api, d1, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	l1.Stop = func() {}
	require.True(t, l1.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	require.NoError(t, l1.Subscribe(ctx, claudeSub("s2"), 5))
	assert.Empty(t, d1.got, "refused after the fence")

	d2 := &fakeDeliverer{}
	l2, err := NewListener(api, d2, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	l2.RecoverAll(ctx)
	require.Len(t, d2.got, 1)
	assert.Contains(t, d2.got[0].text, "context before the watch")
	l2.RecoverAll(ctx)
	assert.Len(t, d2.got, 1, "sent once")
	assert.Empty(t, l2.state.Backlogs)
}

// A cohort tick during shutdown sends no notice, leaves it owed for the next
// listener, and releases its delivery count.
func TestCohortTickDuringShutdown(t *testing.T) {
	f := newCohortFixture(t)
	f.at(2 * time.Hour)
	f.l.Stop = func() {}
	require.True(t, f.l.Control(context.Background(), ControlRequest{Op: "shutdown"}).OK)
	f.tick()
	assert.Empty(t, f.notices("s2"), "no notice after the fence")
	f.l.mu.Lock()
	n := f.l.delivering
	reg := f.l.state.Cohort[cohortKey("s2", "proj")]
	f.l.mu.Unlock()
	assert.Equal(t, 0, n, "the send phase released its count")
	require.NotNil(t, reg)
	assert.True(t, reg.CheckpointNotified.IsZero(), "still owed: the next listener sends it")
}

// A kept backlog goes with its watch: unsubscribing drops it, and a backlog
// whose watch is gone when the next listener starts is discarded.
func TestKeptBacklogEndsWithItsWatch(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.NoError(t, l.Subscribe(ctx, claudeSub("s2"), 0))
	l.mu.Lock()
	l.state.Backlogs = map[string]int{"s2|C1": 5, "gone|C9": 3}
	l.mu.Unlock()
	l.RecoverAll(ctx)
	l.mu.Lock()
	_, stale := l.state.Backlogs["gone|C9"]
	l.mu.Unlock()
	assert.False(t, stale, "a backlog whose watch is gone is discarded")

	l.mu.Lock()
	l.state.Backlogs = map[string]int{"s2|C1": 5}
	l.mu.Unlock()
	l.Unsubscribe("s2", "")
	l.mu.Lock()
	n := len(l.state.Backlogs)
	l.mu.Unlock()
	assert.Equal(t, 0, n, "unsubscribing drops the session's kept backlog")
}

// The next listener sends a kept backlog together with everything newer
// than the join, each message once, and a delivered backlog is cleared.
func TestKeptBacklogIncludesTheGap(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	path := filepath.Join(t.TempDir(), "state.json")
	l1, err := NewListener(api, &fakeDeliverer{}, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	l1.Now = func() time.Time { return time.Unix(2000, 0) }
	l1.Stop = func() {}
	api.history["C1"] = []slack.Message{msg("1999.5", "UBR", "pre-watch")}
	require.True(t, l1.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	require.NoError(t, l1.Subscribe(ctx, claudeSub("s2"), 2))
	require.Equal(t, 2, l1.state.Backlogs["s2|C1"], "refused, so kept")

	api.history["C1"] = []slack.Message{msg("2000.6", "UBR", "gap two"), msg("2000.5", "UBR", "gap one"), msg("1999.5", "UBR", "pre-watch")}
	d2 := &fakeDeliverer{}
	l2, err := NewListener(api, d2, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	l2.RecoverAll(ctx)
	require.Len(t, d2.got, 1)
	text := d2.got[0].text
	assert.Equal(t, 1, strings.Count(text, "pre-watch"), "the backlog counts back from the watch's start, however many messages followed")
	assert.Equal(t, 1, strings.Count(text, "gap one"), "messages since the join follow it")
	assert.Equal(t, 1, strings.Count(text, "gap two"))
	assert.Less(t, strings.Index(text, "pre-watch"), strings.Index(text, "gap one"), "oldest first")
	assert.Empty(t, l2.state.Backlogs, "cleared once delivered")

	d3 := &fakeDeliverer{}
	l3 := newTestListener(t, newFakeSlack(), d3)
	l3.state.Backlogs = nil
	require.NoError(t, l3.Subscribe(ctx, claudeSub("s3"), 2))
	assert.Empty(t, l3.state.Backlogs, "a normal watch keeps nothing once its backlog is settled")
}

// A backlog whose delivery fails is kept and sent by a later retry, once.
func TestFailedBacklogIsRetried(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	api.history["C1"] = []slack.Message{msg("1999.5", "UBR", "context")}
	d := &fakeDeliverer{errs: map[string]error{"s4": errors.New("socket busy")}}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(ctx, claudeSub("s4"), 3))
	assert.Empty(t, d.got)
	assert.Equal(t, 3, l.state.Backlogs["s4|C1"], "kept after the failure")

	l.RetryBacklogs(ctx) // still failing: backs off
	l.mu.Lock()
	r := l.backlogRetry["s4|C1"]
	l.mu.Unlock()
	assert.Equal(t, 1, r.tries)
	assert.WithinDuration(t, time.Now().Add(backlogRetryMin), r.next, 5*time.Second, "the first backoff is the minimum")
	d.mu.Lock()
	d.errs = nil
	d.mu.Unlock()
	l.mu.Lock()
	l.backlogRetry = nil // as if the backoff had elapsed
	l.mu.Unlock()
	l.RetryBacklogs(ctx)
	require.Len(t, d.got, 1)
	assert.Contains(t, d.got[0].text, "context")
	assert.Empty(t, l.state.Backlogs, "cleared once delivered")
	l.RetryBacklogs(ctx)
	assert.Len(t, d.got, 1, "not sent again")
}

// A long backlog is delivered whole, oldest first, in consecutive parts, each
// telling the agent to read every part before acting.
func TestLongBacklogIsDeliveredInParts(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	path := filepath.Join(t.TempDir(), "state.json")
	l1, err := NewListener(api, &fakeDeliverer{}, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	l1.Now = func() time.Time { return time.Unix(2000, 0) }
	l1.Stop = func() {}
	api.history["C1"] = []slack.Message{msg("1999.5", "UBR", "before the watch")}
	require.True(t, l1.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	require.NoError(t, l1.Subscribe(ctx, claudeSub("s2"), 1))

	var hist []slack.Message
	for i := maxRecovery + 9; i >= 0; i-- { // newest first
		hist = append(hist, msg(fmt.Sprintf("%d.000100", 2001+i), "UBR", fmt.Sprintf("gap %03d", i)))
	}
	api.history["C1"] = append(hist, msg("1999.5", "UBR", "before the watch"))
	d2 := &fakeDeliverer{}
	l2, err := NewListener(api, d2, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", path, zap.NewNop())
	require.NoError(t, err)
	l2.RecoverAll(ctx)
	require.Len(t, d2.got, 2, "two parts")
	first, last := d2.got[0].text, d2.got[1].text
	assert.Contains(t, first, "part 1 of 2")
	assert.Contains(t, first, "read every part before acting")
	assert.Contains(t, last, "part 2 of 2")
	assert.Contains(t, last, "last part")
	assert.Contains(t, first, "before the watch", "the oldest message comes first")
	assert.Contains(t, first, "gap 000")
	assert.Contains(t, last, fmt.Sprintf("gap %03d", maxRecovery+9), "the newest comes last")
	all := first + last
	for i := 0; i <= maxRecovery+9; i++ {
		assert.Equal(t, 1, strings.Count(all, fmt.Sprintf("gap %03d", i)), "every message once")
	}
	assert.Less(t, strings.Index(all, "gap 010"), strings.Index(all, "gap 011"), "in order")
}

// Recovery sends nothing from a channel the session stopped watching while
// history was being read.
func TestRecoveryDropsUnwatchedChannels(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	api.history["C1"] = []slack.Message{msg("1999.5", "UBR", "old")}
	d := &fakeDeliverer{}
	l := newTestListener(t, api, d)
	sub := claudeSub("s5")
	require.NoError(t, l.Subscribe(ctx, sub, 0))
	stale := snapshot(l.state.Subscriptions["s5"])
	l.Unsubscribe("s5", "C1")
	l.recover(ctx, stale, stale.Channels, map[string]bool{"C1": true}, 3)
	assert.Empty(t, d.got)
}

// Backoff is per channel: a channel that newly fails is retried on its own
// schedule, not behind another channel's backoff, and unsubscribing clears it.
func TestBacklogBackoffIsPerChannel(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	l.state.Subscriptions["s6"] = &Subscription{SessionID: "s6", Kind: KindClaude, Socket: "/s", Token: "t", Channels: []string{"C1", "C2"}}
	l.state.Backlogs = map[string]int{"s6|C1": 2, "s6|C2": 2}
	l.backlogRetry = map[string]backlogRetry{"s6|C1": {tries: 3, next: time.Now().Add(time.Hour)}}
	api := l.API.(*fakeSlack)
	api.failReads = true
	l.RetryBacklogs(context.Background())
	assert.Equal(t, 3, l.backlogRetry["s6|C1"].tries, "C1 is still backing off: not retried")
	assert.Equal(t, 1, l.backlogRetry["s6|C2"].tries, "C2 was retried on its own schedule")
	l.Unsubscribe("s6", "C2")
	_, kept := l.backlogRetry["s6|C2"]
	assert.False(t, kept, "unsubscribing clears its backoff")
}
