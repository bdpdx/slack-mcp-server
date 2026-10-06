package handler

import (
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/toolconfig"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func policyTestCache() *provider.ChannelsCache {
	return &provider.ChannelsCache{
		Channels: map[string]provider.Channel{
			"C123": {ID: "C123", Name: "#general"},
			"C456": {ID: "C456", Name: "#random"},
			"C789": {ID: "C789", Name: "#ops"},
			"D111": {ID: "D111", Name: "@alice", IsIM: true, User: "U111"},
			"D222": {ID: "D222", Name: "@bob", IsIM: true, User: "U222"},
		},
		ChannelsInv: map[string]string{
			"#general": "C123",
			"#random":  "C456",
			"#ops":     "C789",
			"@alice":   "D111",
			"@bob":     "D222",
		},
	}
}

func newPolicyTestHandler(t *testing.T, env map[string]string) *ConversationsHandler {
	t.Helper()
	cfg, err := toolconfig.FromMap(nil, env)
	require.NoError(t, err)
	return &ConversationsHandler{
		logger:     zap.NewNop(),
		cfg:        cfg,
		channelsFn: policyTestCache,
	}
}

func toolRequest(tool string, args map[string]any) mcp.CallToolRequest {
	r := mcp.CallToolRequest{}
	r.Params.Name = tool
	r.Params.Arguments = args
	return r
}

func TestUnitPolicyAllows(t *testing.T) {
	cache := policyTestCache()
	tests := []struct {
		name    string
		channel string
		config  string
		want    bool
		wantErr bool
	}{
		{"true allows all", "C123", "true", true, false},
		{"yes allows all", "C123", "YES", true, false},
		{"allowlist - channel in list", "C123", "C123,C456", true, false},
		{"allowlist - second channel in list", "C456", "C123,C456", true, false},
		{"allowlist - channel NOT in list", "C789", "C123,C456", false, false},
		{"allowlist - with spaces and lower case", "C123", " c123 , C456 ", true, false},
		{"allowlist - channel name", "C123", "#general", true, false},
		{"allowlist - channel name upper case", "C123", "#General", true, false},
		{"allowlist - channel name other", "C456", "#general", false, false},
		{"allowlist - unknown name never matches", "C123", "#missing", false, false},
		{"allowlist - DM by @name", "D111", "@alice", true, false},
		{"allowlist - DM by user ID", "D111", "U111", true, false},
		{"allowlist - other DM by user ID", "D222", "U111", false, false},
		{"blocklist - channel in list", "C123", "!C123,!C456", false, false},
		{"blocklist - channel NOT in list", "C789", "!C123,!C456", true, false},
		{"blocklist - with spaces", "C123", " !C123 , !C456 ", false, false},
		{"blocklist - channel name", "C123", "!#general", false, false},
		{"blocklist - DM by user ID", "D111", "!U111", false, false},
		{"blocklist - unresolvable name fails closed", "C789", "!#missing", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := toolconfig.ParseChannelPolicy(tt.config)
			require.NoError(t, err)
			got, err := policyAllows(policy, tt.channel, cache)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantErr, err != nil, "err=%v", err)
		})
	}
}

func TestUnitCheckWriteTarget(t *testing.T) {
	t.Run("disabled tool is refused", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{toolconfig.EnvAddMessageTool: "false"})
		_, err := h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "C123")
		require.ErrorContains(t, err, "disabled")
	})

	t.Run("names and lower-case IDs are canonicalised", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{toolconfig.EnvAddMessageTool: "C123"})
		got, err := h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "#general")
		require.NoError(t, err)
		assert.Equal(t, "C123", got)

		got, err = h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, " c123 ")
		require.NoError(t, err)
		assert.Equal(t, "C123", got)
	})

	t.Run("blocked DM cannot be reached by user ID", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{toolconfig.EnvAddMessageTool: "!D111"})
		_, err := h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "U111")
		require.ErrorContains(t, err, "not allowed")

		_, err = h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "u111")
		require.ErrorContains(t, err, "not allowed")

		_, err = h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "@alice")
		require.ErrorContains(t, err, "not allowed")

		got, err := h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "U222")
		require.NoError(t, err)
		assert.Equal(t, "D222", got)
	})

	t.Run("user ID without a known DM is refused under a policy", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{toolconfig.EnvAddMessageTool: "!C123"})
		_, err := h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "U999")
		require.ErrorContains(t, err, "cannot be checked")
	})

	t.Run("user ID passes through when all channels are allowed", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{toolconfig.EnvAddMessageTool: "true"})
		got, err := h.checkWriteTarget(t.Context(), toolconfig.ConversationsAddMessage, "U999")
		require.NoError(t, err)
		assert.Equal(t, "U999", got)
	})

	t.Run("error does not echo the policy", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{toolconfig.EnvDeleteMessageTool: "C456,#random,D222"})
		_, err := h.checkWriteTarget(t.Context(), toolconfig.ConversationsDeleteMessage, "C123")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "C456")
		assert.NotContains(t, err.Error(), "#random")
		assert.NotContains(t, err.Error(), "D222")
	})

	t.Run("each write tool applies its own policy", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{
			toolconfig.EnvAddMessageTool:    "!C123",
			toolconfig.EnvDeleteMessageTool: "!C123",
			toolconfig.EnvReactionTool:      "!C123",
			toolconfig.EnvUploadFileTool:    "!C123",
		})
		for _, tool := range []string{toolconfig.ConversationsAddMessage, toolconfig.ConversationsDeleteMessage, toolconfig.ReactionsAdd, toolconfig.ReactionsRemove, toolconfig.FilesUpload} {
			_, err := h.checkWriteTarget(t.Context(), tool, "#general")
			require.ErrorContains(t, err, "not allowed", tool)
			_, err = h.checkWriteTarget(t.Context(), tool, "C456")
			require.NoError(t, err, tool)
		}
	})
}

func TestUnitAsUserRequiresOptIn(t *testing.T) {
	t.Run("refused by default", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{
			toolconfig.EnvAddMessageTool: "true",
			toolconfig.EnvReactionTool:   "true",
			toolconfig.EnvUploadFileTool: "true",
		})
		_, err := h.parseParamsToolAddMessage(t.Context(), toolRequest("conversations_add_message", map[string]any{"channel_id": "C123", "text": "hi", "as_user": true}))
		require.ErrorContains(t, err, "SLACK_MCP_ALLOW_AS_USER")

		_, err = h.parseParamsToolReaction(t.Context(), toolconfig.ReactionsAdd, toolRequest("reactions_add", map[string]any{"channel_id": "C123", "timestamp": "1.2", "emoji": "x", "as_user": true}))
		require.ErrorContains(t, err, "SLACK_MCP_ALLOW_AS_USER")

		_, err = h.parseParamsToolFilesUpload(t.Context(), toolRequest("files_upload", map[string]any{"channel_id": "C123", "filename": "a.txt", "content": "x", "as_user": true}))
		require.ErrorContains(t, err, "SLACK_MCP_ALLOW_AS_USER")

		params, err := h.parseParamsToolAddMessage(t.Context(), toolRequest("conversations_add_message", map[string]any{"channel_id": "C123", "text": "hi"}))
		require.NoError(t, err)
		assert.False(t, params.asUser)
	})

	t.Run("allowed when enabled", func(t *testing.T) {
		h := newPolicyTestHandler(t, map[string]string{
			toolconfig.EnvAddMessageTool: "true",
			toolconfig.EnvAllowAsUser:    "on",
		})
		params, err := h.parseParamsToolAddMessage(t.Context(), toolRequest("conversations_add_message", map[string]any{"channel_id": "C123", "text": "hi", "as_user": true}))
		require.NoError(t, err)
		assert.True(t, params.asUser)
	})
}

func TestUnitToolEnabledChecksInHandlers(t *testing.T) {
	h := newPolicyTestHandler(t, nil)
	for _, tool := range []string{
		toolconfig.ConversationsJoin, toolconfig.ConversationsLeave, toolconfig.ConversationsRename,
		toolconfig.ConversationsCreate, toolconfig.ConversationsSetTopic, toolconfig.ConversationsInvite,
		toolconfig.ConversationsInviteShared, toolconfig.ConversationsOpen, toolconfig.ConversationsMark,
		toolconfig.AttachmentGetData,
	} {
		require.Error(t, h.checkToolEnabled(tool), tool)
	}

	h = newPolicyTestHandler(t, map[string]string{toolconfig.EnvJoinTool: "true"})
	require.NoError(t, h.checkToolEnabled(toolconfig.ConversationsJoin))
	require.NoError(t, h.checkToolEnabled(toolconfig.ConversationsLeave))

	// Handlers refuse disabled tools before touching Slack.
	h = newPolicyTestHandler(t, nil)
	_, err := h.ConversationsJoinHandler(t.Context(), toolRequest("conversations_join", map[string]any{"channel_id": "C123"}))
	require.ErrorContains(t, err, "SLACK_MCP_JOIN_TOOL")
	_, err = h.ConversationsLeaveHandler(t.Context(), toolRequest("conversations_leave", map[string]any{"channel_id": "C123"}))
	require.ErrorContains(t, err, "SLACK_MCP_JOIN_TOOL")
}

func TestUnitUsergroupsWriteGating(t *testing.T) {
	cfg, err := toolconfig.FromMap(nil, nil)
	require.NoError(t, err)
	h := &UsergroupsHandler{logger: zap.NewNop(), cfg: cfg}
	for _, call := range []func() error{
		func() error {
			_, err := h.UsergroupsCreateHandler(t.Context(), toolRequest("usergroups_create", map[string]any{"name": "x"}))
			return err
		},
		func() error {
			_, err := h.UsergroupsUpdateHandler(t.Context(), toolRequest("usergroups_update", map[string]any{"usergroup_id": "S1"}))
			return err
		},
		func() error {
			_, err := h.UsergroupsUsersUpdateHandler(t.Context(), toolRequest("usergroups_users_update", map[string]any{"usergroup_id": "S1", "users": "U1"}))
			return err
		},
	} {
		require.ErrorContains(t, call(), "SLACK_MCP_USERGROUPS_WRITE_TOOL")
	}

	assert.False(t, cfg.UsergroupsMeWriteEnabled())
	cfg, err = toolconfig.FromMap(nil, map[string]string{toolconfig.EnvUsergroupsWriteTool: "1"})
	require.NoError(t, err)
	assert.True(t, cfg.UsergroupsMeWriteEnabled())
	assert.True(t, cfg.ToolEnabled(toolconfig.UsergroupsCreate))
}
