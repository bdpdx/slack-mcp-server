package handler

import (
	"encoding/base64"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/toolconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitClampInt(t *testing.T) {
	assert.Equal(t, 1, clampInt(-5, 1, 10))
	assert.Equal(t, 1, clampInt(0, 1, 10))
	assert.Equal(t, 7, clampInt(7, 1, 10))
	assert.Equal(t, 10, clampInt(1<<40, 1, 10))
}

func TestUnitLimitByNumericClamps(t *testing.T) {
	n, err := limitByNumeric("-5", defaultConversationsNumericLimit)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	n, err = limitByNumeric("1000000", defaultConversationsNumericLimit)
	require.NoError(t, err)
	assert.Equal(t, maxConversationsNumericLimit, n)

	_, _, _, err = limitByExpression("99999d", defaultConversationsExpressionLimit)
	require.Error(t, err)
}

func TestUnitParseParamsClampNumbers(t *testing.T) {
	h := newPolicyTestHandler(t, nil)

	unreads := h.parseParamsToolUnreads(toolRequest("conversations_unreads", map[string]any{
		"max_channels": -3, "max_messages_per_channel": 100000,
	}))
	assert.Equal(t, 1, unreads.maxChannels)
	assert.Equal(t, maxUnreadsMessagesPerChannel, unreads.maxMessagesPerChannel)

	users, err := h.parseParamsToolUsersSearch(toolRequest("users_search", map[string]any{"query": "a", "limit": -1}))
	require.NoError(t, err)
	assert.Equal(t, 1, users.limit)

	search, err := h.parseParamsToolSearch(t.Context(), toolRequest("conversations_search_messages", map[string]any{"search_query": "x", "limit": -10}))
	require.NoError(t, err)
	assert.Equal(t, 1, search.limit)

	search, err = h.parseParamsToolSearch(t.Context(), toolRequest("conversations_search_messages", map[string]any{"search_query": "x", "limit": 5000}))
	require.NoError(t, err)
	assert.Equal(t, maxSearchLimit, search.limit)

	badCursor := base64.StdEncoding.EncodeToString([]byte("page:100000"))
	_, err = h.parseParamsToolSearch(t.Context(), toolRequest("conversations_search_messages", map[string]any{"search_query": "x", "cursor": badCursor}))
	require.Error(t, err)
}

func TestUnitPaginateChannelsNoPanics(t *testing.T) {
	all := filterChannelsByTypes(testChannelsMap(), []string{"public_channel"})
	page, _ := paginateChannels(all, "", -5)
	assert.Len(t, page, 1)

	pastEnd := base64.StdEncoding.EncodeToString([]byte("ZZZZ"))
	page, cursor := paginateChannels(all, pastEnd, 10)
	assert.Empty(t, page)
	assert.Empty(t, cursor)
}

func TestUnitParseParamsToolMarkResolvesNames(t *testing.T) {
	h := newPolicyTestHandler(t, nil)
	_, err := h.parseParamsToolMark(t.Context(), toolRequest("conversations_mark", map[string]any{"channel_id": "#general"}))
	require.ErrorContains(t, err, "SLACK_MCP_MARK_TOOL")

	h = newPolicyTestHandler(t, map[string]string{toolconfig.EnvMarkTool: "true"})
	params, err := h.parseParamsToolMark(t.Context(), toolRequest("conversations_mark", map[string]any{"channel_id": "#general", "ts": "1.2"}))
	require.NoError(t, err)
	assert.Equal(t, "C123", params.channel)
	assert.Equal(t, "1.2", params.ts)

	_, err = h.parseParamsToolMark(t.Context(), toolRequest("conversations_mark", map[string]any{"channel_id": "#missing"}))
	require.ErrorContains(t, err, "not found")
}
