package agentchat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	v, err := replaceWith(h.probe, h.stop, h.start, "new", "log")
	require.NoError(t, err)
	assert.Equal(t, "new", v)
	assert.Equal(t, 1, h.stops)

	h = &fakeHome{running: "old", racers: []string{"old"}}
	v, err = replaceWith(h.probe, h.stop, h.start, "new", "log")
	require.NoError(t, err, "an older listener that won the gap is replaced again")
	assert.Equal(t, "new", v)
	assert.Equal(t, 2, h.stops)

	h = &fakeHome{running: "old", racers: []string{"old", "old", "old"}}
	_, err = replaceWith(h.probe, h.stop, h.start, "new", "log")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "old, not this binary's new")

	dies := &fakeHome{running: "old"}
	_, err = replaceWith(dies.probe, dies.stop, func() error { dies.starts++; return nil }, "new", "log")
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
	v, err := replaceWith(h.probe, h.stop, start, "new", "RECOVER")
	require.NoError(t, err)
	assert.Equal(t, "new", v)

	h = &fakeHome{running: "old"}
	_, err = replaceWith(h.probe, h.stop, func() error { return errors.New("auth.test failed") }, "new", "RECOVER")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not start")
	assert.Contains(t, err.Error(), "RECOVER")
}

// Shutdown waits for a hook to collect an answer the owner already gave.
func TestShutdownWaitsForAnswers(t *testing.T) {
	ctx := context.Background()
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	require.True(t, l.Control(ctx, ControlRequest{Op: "approval-watch", Approval: "a", Channel: "C1", TS: "2000.1"}).OK)
	l.HandleInteraction(clickOn("UBR", decisionAllow, "a", "C1", "2000.1", "BCL"))
	stopped := make(chan struct{})
	l.Stop = func() { close(stopped) }
	require.True(t, l.Control(ctx, ControlRequest{Op: "shutdown"}).OK)
	select {
	case <-stopped:
		t.Fatal("stopped while an answer was waiting for its hook")
	case <-time.After(500 * time.Millisecond):
	}
	d, _ := takeApproval(t, l, "a")
	assert.Equal(t, decisionAllow, d)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("never stopped after the answer was collected")
	}
}
