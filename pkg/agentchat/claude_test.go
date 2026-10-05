package agentchat

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortSocketPath returns a Unix socket path short enough for macOS.
func shortSocketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "sac")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestDeliverClaude(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer ln.Close()
	lines := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		var got []string
		for sc.Scan() {
			got = append(got, sc.Text())
		}
		lines <- got
	}()

	require.NoError(t, DeliverClaude(context.Background(), path, "tok", "hello\nworld"))

	got := <-lines
	require.Len(t, got, 2)
	var auth map[string]string
	require.NoError(t, json.Unmarshal([]byte(got[0]), &auth))
	assert.Equal(t, map[string]string{"type": "auth", "token": "tok"}, auth)
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(got[1]), &msg))
	assert.Equal(t, "user", msg.Type)
	assert.Equal(t, "user", msg.Message.Role)
	assert.Equal(t, "hello\nworld", msg.Message.Content)
}

func TestDeliverClaudeGone(t *testing.T) {
	err := DeliverClaude(context.Background(), shortSocketPath(t), "tok", "x")
	assert.ErrorIs(t, err, ErrSessionGone)
}
