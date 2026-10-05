package handler

import (
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Agents rely on the message's text field to see @mentions in each other's
// replies, so markdown posts must carry the source text alongside their blocks.
func TestUnitContentOptionsMarkdownKeepsText(t *testing.T) {
	opts, err := contentOptions("@codex-r **done**", "text/markdown", nil, zap.NewNop())
	require.NoError(t, err)
	_, values, err := slack.UnsafeApplyMsgOptions("", "C1", "", opts...)
	require.NoError(t, err)
	assert.Equal(t, "@codex-r **done**", values.Get("text"))
	assert.NotEmpty(t, values.Get("blocks"))
}

func TestUnitContentOptionsPlain(t *testing.T) {
	opts, err := contentOptions("hi", "text/plain", nil, zap.NewNop())
	require.NoError(t, err)
	_, values, err := slack.UnsafeApplyMsgOptions("", "C1", "", opts...)
	require.NoError(t, err)
	assert.Equal(t, "hi", values.Get("text"))
	assert.Empty(t, values.Get("blocks"))
}

func TestUnitContentOptionsRejectsUnknownType(t *testing.T) {
	_, err := contentOptions("hi", "text/html", nil, zap.NewNop())
	assert.Error(t, err)
}
