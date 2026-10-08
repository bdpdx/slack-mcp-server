package agentchat

import (
	"context"
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
	assert.Contains(t, err.Error(), "none")
}
