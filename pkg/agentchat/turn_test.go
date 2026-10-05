package agentchat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTurnMarks(t *testing.T) {
	home := NewHome(filepath.Join(t.TempDir(), EnvFileName))

	require.NoError(t, MarkTurn(home, "s1", "fix the bug"))
	assert.True(t, TakeTurnMark(home, "s1"), "typed at the terminal")
	assert.False(t, TakeTurnMark(home, "s1"), "taking clears the mark")

	require.NoError(t, MarkTurn(home, "s1", "fix the bug"))
	require.NoError(t, MarkTurn(home, "s1", "[slack-agent-chat] #proj (C1) from codex-b, ts 1.2:\n> hi"))
	assert.False(t, TakeTurnMark(home, "s1"), "a Slack notice starting the turn clears it")

	require.NoError(t, MarkTurn(home, "s1", "x"))
	assert.False(t, TakeTurnMark(home, "s2"), "marks are per session")

	assert.Error(t, MarkTurn(home, "../evil", "x"))
	assert.Error(t, MarkTurn(home, "", "x"))
	assert.False(t, TakeTurnMark(home, "../evil"))
}

func TestSplitMarkdown(t *testing.T) {
	assert.Equal(t, []string{"short\ntext"}, SplitMarkdown("short\ntext", 100))

	text := strings.Repeat("line of prose\n", 20)
	chunks := SplitMarkdown(text, 60)
	assert.Equal(t, text, strings.Join(chunks, ""), "plain text splits losslessly")
	for _, c := range chunks {
		assert.LessOrEqual(t, len([]rune(c)), 60)
	}

	code := "intro\n```go\n" + strings.Repeat("x := 1\n", 20) + "```\nafter\n"
	chunks = SplitMarkdown(code, 50)
	require.Greater(t, len(chunks), 1)
	for i, c := range chunks {
		assert.LessOrEqual(t, len([]rune(c)), 50, "chunk %d", i)
		assert.Equal(t, 0, strings.Count(c, "```")%2, "chunk %d has balanced fences: %q", i, c)
	}
	assert.True(t, strings.HasPrefix(chunks[1], "```go\n"), "the fence reopens with its language")

	long := strings.Repeat("y", 130)
	chunks = SplitMarkdown(long, 50)
	assert.Equal(t, long, strings.Join(chunks, ""), "an overlong line is hard-split")
	for _, c := range chunks {
		assert.LessOrEqual(t, len([]rune(c)), 50)
	}
}
