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
