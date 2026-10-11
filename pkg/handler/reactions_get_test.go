package handler

import (
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitParseParamsToolReactionsGet(t *testing.T) {
	handler := newPolicyTestHandler(t, nil)

	params, err := handler.parseParamsToolReactionsGet(t.Context(), toolRequest("reactions_get", map[string]any{"channel_id": "C123", "timestamp": "1234567890.123456"}))
	require.NoError(t, err)
	assert.Equal(t, "C123", params.channel)
	assert.Equal(t, "1234567890.123456", params.timestamp)

	_, err = handler.parseParamsToolReactionsGet(t.Context(), toolRequest("reactions_get", map[string]any{"timestamp": "1234567890.123456"}))
	assert.ErrorContains(t, err, "channel_id")
	_, err = handler.parseParamsToolReactionsGet(t.Context(), toolRequest("reactions_get", map[string]any{"channel_id": "C123"}))
	assert.ErrorContains(t, err, "timestamp")
	_, err = handler.parseParamsToolReactionsGet(t.Context(), toolRequest("reactions_get", map[string]any{"channel_id": "C123", "timestamp": "yesterday"}))
	assert.ErrorContains(t, err, "timestamp")
}

// One row per emoji and reacting user, named where the user is known.
func TestUnitReactionRows(t *testing.T) {
	names := map[string][2]string{"U1": {"codexb", "codex-b"}}
	resolve := func(id string) (string, string, bool) {
		n, ok := names[id]
		return n[0], n[1], ok
	}
	rows := reactionRows([]slack.ItemReaction{
		{Name: "eyes", Count: 2, Users: []string{"U1", "U2"}},
		{Name: "white_check_mark", Count: 1, Users: []string{"U1"}},
	}, resolve)
	assert.Equal(t, []ReactionRow{
		{Emoji: "eyes", UserID: "U1", UserName: "codexb", RealName: "codex-b"},
		{Emoji: "eyes", UserID: "U2", UserName: "U2", RealName: "U2"},
		{Emoji: "white_check_mark", UserID: "U1", UserName: "codexb", RealName: "codex-b"},
	}, rows)
}
