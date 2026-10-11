package agentchat

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcAlive(t *testing.T) {
	start, ok := procStart(os.Getpid())
	require.True(t, ok)
	assert.True(t, procAlive(os.Getpid(), start))
	assert.False(t, procAlive(os.Getpid(), start+1), "same PID, different start: a reused PID")

	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	assert.False(t, procAlive(cmd.Process.Pid, 0), "an exited process")
}

// The exit watch fires when the process exits, without polling for it.
func TestWatchExit(t *testing.T) {
	cmd := exec.Command("sleep", "0.3")
	require.NoError(t, cmd.Start())
	go func() { _ = cmd.Wait() }()
	start, ok := procStart(cmd.Process.Pid)
	require.True(t, ok)
	exited := make(chan struct{})
	require.True(t, watchExit(cmd.Process.Pid, start, func() { close(exited) }), "supported on this OS")
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("no exit event")
	}

	gone := make(chan struct{})
	watchExit(cmd.Process.Pid, start, func() { close(gone) }) // already exited: fires at once
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("no event for an exited process")
	}
}
