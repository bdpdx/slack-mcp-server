package agentchat

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlRoundTrip(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := ListenControl(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ServeControl(ctx, ln, func(_ context.Context, req ControlRequest) ControlResponse {
		if req.Op == "status" {
			return ControlResponse{OK: true, Sessions: []SessionStatus{{SessionID: "s", Kind: KindCodex, Channels: []string{"C1"}}}}
		}
		return ControlResponse{Error: "unknown op " + req.Op}
	})

	resp, err := SendControl(ctx, path, ControlRequest{Op: "status"})
	require.NoError(t, err)
	assert.Equal(t, "s", resp.Sessions[0].SessionID)

	_, err = SendControl(ctx, path, ControlRequest{Op: "bogus"})
	assert.ErrorContains(t, err, "unknown op bogus")

	_, err = ListenControl(path)
	assert.ErrorIs(t, err, ErrListenerRunning)
}

func TestListenControlReplacesStaleSocket(t *testing.T) {
	path := shortSocketPath(t)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	ln, err := ListenControl(path)
	require.NoError(t, err)
	ln.Close()
}
