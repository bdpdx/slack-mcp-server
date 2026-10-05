package agentchat

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeChannelName(t *testing.T) {
	got, err := NormalizeChannelName("2026.09.21.Backup Restore")
	require.NoError(t, err)
	assert.Equal(t, "2026-09-21-backup-restore", got)
	got, err = NormalizeChannelName("#Proj__X")
	require.NoError(t, err)
	assert.Equal(t, "proj__x", got)
	long, err := NormalizeChannelName(strings.Repeat("a", 90))
	require.NoError(t, err)
	assert.Len(t, long, 80)
	_, err = NormalizeChannelName("...")
	assert.Error(t, err)
}

func TestParseRelayPrompt(t *testing.T) {
	target, text, ok := ParseRelayPrompt("  %agents: ship it\nnow")
	assert.True(t, ok)
	assert.Equal(t, "", target)
	assert.Equal(t, "ship it\nnow", text)

	target, text, ok = ParseRelayPrompt("%agents@proj: hi")
	assert.True(t, ok)
	assert.Equal(t, "proj", target)
	assert.Equal(t, "hi", text)

	_, _, ok = ParseRelayPrompt("please %agents: no")
	assert.False(t, ok)
}

func TestDetectSession(t *testing.T) {
	sub, err := detectSession(envMap(map[string]string{"CODEX_THREAD_ID": "th"}))
	require.NoError(t, err)
	assert.Equal(t, &Subscription{SessionID: "th", Kind: KindCodex, ThreadID: "th"}, sub)

	sub, err = detectSession(envMap(map[string]string{
		"CLAUDE_CODE_SESSION_ID": "cs", "CLAUDE_CODE_MESSAGING_SOCKET": "/s", "CLAUDE_CODE_MESSAGING_TOKEN": "tok",
	}))
	require.NoError(t, err)
	assert.Equal(t, &Subscription{SessionID: "cs", Kind: KindClaude, Socket: "/s", Token: "tok"}, sub)

	_, err = detectSession(envMap(map[string]string{"CLAUDE_CODE_SESSION_ID": "cs"}))
	assert.ErrorContains(t, err, "CLAUDE_CODE_MESSAGING_SOCKET")
	_, err = detectSession(envMap(nil))
	assert.Error(t, err)
}

func TestRelayHookIgnoresOrdinaryPromptsEvenWithoutEnvFile(t *testing.T) {
	var out, errOut strings.Builder
	code := RunCLI([]string{"--env-file", "/nonexistent/x.env", "relay-hook"},
		strings.NewReader(`{"prompt":"just a normal prompt","session_id":"s"}`), &out, &errOut)
	assert.Equal(t, 0, code)
	assert.Empty(t, out.String(), "a non-%agents prompt must never be blocked")

	out.Reset()
	code = RunCLI([]string{"--env-file", "/nonexistent/x.env", "relay-hook"},
		strings.NewReader(`{"prompt":"%agents: hi","session_id":"s"}`), &out, &errOut)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), `"decision":"block"`)
}

func TestHelpListsCommandsAndSucceeds(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var out, errOut strings.Builder
		code := RunCLI(args, strings.NewReader(""), &out, &errOut)
		assert.Equal(t, 0, code, "%v", args)
		help := out.String() + errOut.String()
		for _, want := range []string{"watch start", "watch stop", "watch status", "channel create", "channel invite", "post", "ack CHANNEL TS", "relay-hook", "--env-file"} {
			assert.Contains(t, help, want, "%v", args)
		}
	}
}

// SAC-4: a stuck listener must not hold a %agents prompt hostage.
func TestRelayHookGivesUpAfterTimeout(t *testing.T) {
	old := relayTimeout
	relayTimeout = 200 * time.Millisecond
	defer func() { relayTimeout = old }()
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "")

	dir, err := os.MkdirTemp("/tmp", "sac")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	env := filepath.Join(dir, EnvFileName)
	require.NoError(t, os.WriteFile(env, []byte("SLACK_MCP_XOXB_TOKEN=xoxb-x\nSLACK_MCP_XOXP_TOKEN=xoxp-x\n"), 0o600))
	home := NewHome(env)
	require.NoError(t, os.MkdirAll(home.StateDir, 0o700))
	ln, err := net.Listen("unix", home.ControlSocket)
	require.NoError(t, err)
	defer ln.Close()
	var held []net.Conn
	go func() { // accepts, never answers
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()

	var out, errOut strings.Builder
	start := time.Now()
	code := RunCLI([]string{"--env-file", env, "relay-hook"},
		strings.NewReader(`{"prompt":"%agents: hi","session_id":"s"}`), &out, &errOut)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), `"decision":"block"`)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestRelayContext(t *testing.T) {
	got := relayContext("proj", "5.0")
	assert.Contains(t, got, "#proj")
	assert.Contains(t, got, "do not send it again")
	assert.Contains(t, got, "Carry out that text yourself")
}
