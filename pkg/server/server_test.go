package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/bdpdx/slack-mcp-server/pkg/toolconfig"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func toolEnabled(t *testing.T, enabledTools []string, env map[string]string, tool string) bool {
	t.Helper()
	cfg, err := toolconfig.FromMap(enabledTools, env)
	require.NoError(t, err)
	return cfg.ToolEnabled(tool)
}

func TestUnitToolEnabled_ReadOnly_EmptyEnabledTools(t *testing.T) {
	readOnlyTools := []string{
		ToolConversationsHistory,
		ToolConversationsReplies,
		ToolConversationsSearchMessages,
		ToolConversationsUnreads,
		ToolChannelsList,
		ToolChannelsMe,
		ToolUsergroupsList,
		ToolUsergroupsMe,
		ToolUsersSearch,
	}
	for _, tool := range readOnlyTools {
		assert.True(t, toolEnabled(t, nil, nil, tool), "tool %s should be registered when enabledTools is empty", tool)
	}
}

func TestUnitToolEnabled_ReadOnly_ExplicitEnabledTools(t *testing.T) {
	enabled := []string{ToolConversationsHistory, ToolChannelsList}
	assert.True(t, toolEnabled(t, enabled, nil, ToolConversationsHistory))
	assert.False(t, toolEnabled(t, enabled, nil, ToolConversationsAddMessage))
	assert.False(t, toolEnabled(t, []string{ToolChannelsList}, nil, ToolConversationsHistory))
}

func TestUnitToolEnabled_SingleToolEnabled(t *testing.T) {
	for _, tool := range ValidToolNames {
		result := toolEnabled(t, []string{ToolChannelsList}, nil, tool)
		if tool == ToolChannelsList {
			assert.True(t, result, "channels_list should be registered")
		} else {
			assert.False(t, result, "%s should NOT be registered when only channels_list is enabled", tool)
		}
	}
}

func TestUnitWriteToolsAreOffByDefault(t *testing.T) {
	gated := map[string]bool{}
	for _, tool := range toolconfig.GatedTools() {
		gated[tool] = true
		assert.Contains(t, ValidToolNames, tool, "gated tool %s must be a valid tool name", tool)
		assert.False(t, toolEnabled(t, nil, nil, tool), "%s must be off with no configuration", tool)
	}
	// Every tool that changes something must be gated.
	for _, tool := range []string{
		ToolConversationsAddMessage, ToolConversationsDeleteMessage, ToolConversationsOpen,
		ToolFilesUpload, ToolReactionsAdd, ToolReactionsRemove, ToolConversationsMark,
		ToolConversationsLeave, ToolConversationsJoin, ToolConversationsRename,
		ToolConversationsCreate, ToolConversationsSetTopic, ToolConversationsInvite,
		ToolConversationsInviteShared, ToolUsergroupsCreate, ToolUsergroupsUpdate,
		ToolUsergroupsUsersUpdate,
	} {
		assert.True(t, gated[tool], "%s changes Slack state and must be gated", tool)
	}
}

func TestUnitToolEnabled_EnvVars(t *testing.T) {
	tests := []struct {
		tool string
		env  string
	}{
		{ToolConversationsJoin, "SLACK_MCP_JOIN_TOOL"},
		{ToolConversationsLeave, "SLACK_MCP_JOIN_TOOL"},
		{ToolUsergroupsCreate, "SLACK_MCP_USERGROUPS_WRITE_TOOL"},
		{ToolUsergroupsUpdate, "SLACK_MCP_USERGROUPS_WRITE_TOOL"},
		{ToolUsergroupsUsersUpdate, "SLACK_MCP_USERGROUPS_WRITE_TOOL"},
		{ToolConversationsRename, "SLACK_MCP_RENAME_CHANNEL_TOOL"},
		{ToolConversationsCreate, "SLACK_MCP_CREATE_CHANNEL_TOOL"},
		{ToolConversationsSetTopic, "SLACK_MCP_SET_TOPIC_TOOL"},
		{ToolConversationsInvite, "SLACK_MCP_INVITE_TOOL"},
		{ToolConversationsInviteShared, "SLACK_MCP_INVITE_SHARED_TOOL"},
		{ToolConversationsOpen, "SLACK_MCP_OPEN_CONVERSATION_TOOL"},
		{ToolConversationsMark, "SLACK_MCP_MARK_TOOL"},
		{ToolAttachmentGetData, "SLACK_MCP_ATTACHMENT_TOOL"},
		{ToolConversationsAddMessage, "SLACK_MCP_ADD_MESSAGE_TOOL"},
		{ToolConversationsDeleteMessage, "SLACK_MCP_DELETE_MESSAGE_TOOL"},
		{ToolReactionsAdd, "SLACK_MCP_REACTION_TOOL"},
		{ToolReactionsRemove, "SLACK_MCP_REACTION_TOOL"},
		{ToolFilesUpload, "SLACK_MCP_UPLOAD_FILE_TOOL"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			for _, on := range []string{"true", "1", "yes", "on", " TRUE ", "On"} {
				assert.True(t, toolEnabled(t, nil, map[string]string{tt.env: on}, tt.tool), "%s=%q", tt.env, on)
			}
			for _, off := range []string{"", "false", "0", "no", "off", "FALSE", " Off "} {
				assert.False(t, toolEnabled(t, nil, map[string]string{tt.env: off}, tt.tool), "%s=%q", tt.env, off)
			}
			// An explicit off value wins over the enabled-tools list.
			assert.False(t, toolEnabled(t, []string{tt.tool}, map[string]string{tt.env: "false"}, tt.tool))
			// Unset variable plus an explicit listing enables it.
			assert.True(t, toolEnabled(t, []string{tt.tool}, nil, tt.tool))
		})
	}
}

func TestValidToolNames(t *testing.T) {
	t.Run("ValidToolNames contains all expected tools", func(t *testing.T) {
		expectedTools := map[string]bool{
			ToolConversationsHistory:        true,
			ToolConversationsReplies:        true,
			ToolConversationsAddMessage:     true,
			ToolConversationsDeleteMessage:  true,
			ToolConversationsOpen:           true,
			ToolFilesUpload:                 true,
			ToolReactionsAdd:                true,
			ToolReactionsRemove:             true,
			ToolAttachmentGetData:           true,
			ToolConversationsSearchMessages: true,
			ToolConversationsUnreads:        true,
			ToolConversationsMark:           true,
			ToolConversationsLeave:          true,
			ToolConversationsJoin:           true,
			ToolConversationsRename:         true,
			ToolConversationsCreate:         true,
			ToolConversationsSetTopic:       true,
			ToolConversationsInvite:         true,
			ToolConversationsInviteShared:   true,
			ToolChannelsList:                true,
			ToolChannelsMe:                  true,
			ToolUsergroupsList:              true,
			ToolUsergroupsMe:                true,
			ToolUsergroupsCreate:            true,
			ToolUsergroupsUpdate:            true,
			ToolUsergroupsUsersUpdate:       true,
			ToolUsersSearch:                 true,
		}

		assert.Equal(t, len(expectedTools), len(ValidToolNames), "ValidToolNames should have %d tools", len(expectedTools))

		for _, tool := range ValidToolNames {
			assert.True(t, expectedTools[tool], "unexpected tool in ValidToolNames: %s", tool)
		}
	})

	t.Run("constants match their string values", func(t *testing.T) {
		assert.Equal(t, "conversations_history", ToolConversationsHistory)
		assert.Equal(t, "conversations_replies", ToolConversationsReplies)
		assert.Equal(t, "conversations_add_message", ToolConversationsAddMessage)
		assert.Equal(t, "files_upload", ToolFilesUpload)
		assert.Equal(t, "reactions_add", ToolReactionsAdd)
		assert.Equal(t, "reactions_remove", ToolReactionsRemove)
		assert.Equal(t, "attachment_get_data", ToolAttachmentGetData)
		assert.Equal(t, "conversations_search_messages", ToolConversationsSearchMessages)
		assert.Equal(t, "conversations_unreads", ToolConversationsUnreads)
		assert.Equal(t, "conversations_mark", ToolConversationsMark)
		assert.Equal(t, "conversations_leave", ToolConversationsLeave)
		assert.Equal(t, "conversations_join", ToolConversationsJoin)
		assert.Equal(t, "conversations_rename", ToolConversationsRename)
		assert.Equal(t, "conversations_create", ToolConversationsCreate)
		assert.Equal(t, "conversations_set_topic", ToolConversationsSetTopic)
		assert.Equal(t, "conversations_invite", ToolConversationsInvite)
		assert.Equal(t, "conversations_invite_shared", ToolConversationsInviteShared)
		assert.Equal(t, "channels_list", ToolChannelsList)
		assert.Equal(t, "channels_me", ToolChannelsMe)
		assert.Equal(t, "usergroups_list", ToolUsergroupsList)
		assert.Equal(t, "usergroups_me", ToolUsergroupsMe)
		assert.Equal(t, "usergroups_create", ToolUsergroupsCreate)
		assert.Equal(t, "usergroups_update", ToolUsergroupsUpdate)
		assert.Equal(t, "usergroups_users_update", ToolUsergroupsUsersUpdate)
		assert.Equal(t, "users_search", ToolUsersSearch)
	})
}

func TestValidateEnabledTools(t *testing.T) {
	t.Run("empty list is valid", func(t *testing.T) {
		err := ValidateEnabledTools([]string{})
		assert.NoError(t, err)
	})

	t.Run("nil list is valid", func(t *testing.T) {
		err := ValidateEnabledTools(nil)
		assert.NoError(t, err)
	})

	t.Run("all valid tool names pass", func(t *testing.T) {
		err := ValidateEnabledTools(ValidToolNames)
		assert.NoError(t, err)
	})

	t.Run("single valid tool passes", func(t *testing.T) {
		err := ValidateEnabledTools([]string{ToolChannelsList})
		assert.NoError(t, err)
	})

	t.Run("single invalid tool fails", func(t *testing.T) {
		err := ValidateEnabledTools([]string{"invalid_tool"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid_tool")
		assert.Contains(t, err.Error(), "Valid tools are:")
	})

	t.Run("multiple invalid tools listed in error", func(t *testing.T) {
		err := ValidateEnabledTools([]string{"foo", "bar"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "foo")
		assert.Contains(t, err.Error(), "bar")
	})

	t.Run("mix of valid and invalid tools fails", func(t *testing.T) {
		err := ValidateEnabledTools([]string{ToolChannelsList, "invalid_tool", ToolReactionsAdd})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid tool name(s): invalid_tool.")
	})

	t.Run("typo in tool name fails", func(t *testing.T) {
		err := ValidateEnabledTools([]string{"channel_list"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "channel_list")
	})
}

func TestUnitLoggableToolParamsRedactsUploadContent(t *testing.T) {
	request := mcp.CallToolRequest{}
	request.Params.Name = ToolFilesUpload
	request.Params.Arguments = map[string]any{
		"channel_id":      "D123",
		"filename":        "recap.html",
		"content":         "private file body",
		"initial_comment": "private comment",
	}

	logged := loggableToolParams(request).(map[string]any)
	assert.Equal(t, "D123", logged["channel_id"])
	assert.Equal(t, len("private file body"), logged["content_bytes"])
	assert.Equal(t, true, logged["content_redacted"])
	assert.Equal(t, true, logged["initial_comment_redacted"])
	assert.NotContains(t, fmt.Sprint(logged), "private file body")
	assert.NotContains(t, fmt.Sprint(logged), "private comment")
}

// setupMCPClientServer creates an MCP server with the given options and tool handler,
// wires up a client via stdio pipes, and returns the connected client.
func setupMCPClientServer(t *testing.T, opts []server.ServerOption, toolHandler server.ToolHandlerFunc) *client.Client {
	t.Helper()

	mcpSrv := server.NewMCPServer("test", "1.0.0", opts...)
	mcpSrv.AddTool(mcp.NewTool("test_tool",
		mcp.WithDescription("A test tool"),
	), toolHandler)

	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stdioSrv := server.NewStdioServer(mcpSrv)
	go func() {
		_ = stdioSrv.Listen(ctx, serverReader, serverWriter)
	}()

	var logBuf bytes.Buffer
	tr := transport.NewIO(clientReader, clientWriter, io.NopCloser(&logBuf))
	err := tr.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })

	c := client.NewClient(tr)

	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	_, err = c.Initialize(ctx, initReq)
	require.NoError(t, err)

	return c
}

func TestIntegrationErrorRecoveryMiddleware(t *testing.T) {
	logger := zap.NewNop()

	t.Run("handler error is converted to isError tool result", func(t *testing.T) {
		c := setupMCPClientServer(t,
			[]server.ServerOption{server.WithToolHandlerMiddleware(buildErrorRecoveryMiddleware(logger))},
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, fmt.Errorf("simulated tool error: invalid channel ID")
			},
		)

		var callReq mcp.CallToolRequest
		callReq.Params.Name = "test_tool"
		result, err := c.CallTool(context.Background(), callReq)

		require.NoError(t, err, "should not return a JSON-RPC error")
		require.NotNil(t, result)
		assert.True(t, result.IsError, "result should have isError=true")
		require.Len(t, result.Content, 1)
		textContent, ok := result.Content[0].(mcp.TextContent)
		require.True(t, ok, "content should be TextContent")
		assert.Contains(t, textContent.Text, "simulated tool error: invalid channel ID")
	})

	t.Run("without middleware handler error becomes JSON-RPC error", func(t *testing.T) {
		c := setupMCPClientServer(t,
			nil, // no error recovery middleware
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, fmt.Errorf("simulated tool error: invalid channel ID")
			},
		)

		var callReq mcp.CallToolRequest
		callReq.Params.Name = "test_tool"
		result, err := c.CallTool(context.Background(), callReq)

		assert.Error(t, err, "should return a JSON-RPC error without middleware")
		assert.Nil(t, result)
	})

	t.Run("successful tool call passes through unchanged", func(t *testing.T) {
		c := setupMCPClientServer(t,
			[]server.ServerOption{server.WithToolHandlerMiddleware(buildErrorRecoveryMiddleware(logger))},
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("all good"), nil
			},
		)

		var callReq mcp.CallToolRequest
		callReq.Params.Name = "test_tool"
		result, err := c.CallTool(context.Background(), callReq)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.False(t, result.IsError, "successful result should not have isError=true")
		require.Len(t, result.Content, 1)
		textContent, ok := result.Content[0].(mcp.TextContent)
		require.True(t, ok)
		assert.Equal(t, "all good", textContent.Text)
	})
}

func TestUnitToolEnabled_Matrix(t *testing.T) {
	// | ENABLED_TOOLS | TOOL_ENV_VAR | Result |
	// |---------------|--------------|--------|
	// | empty         | empty        | NOT registered |
	// | empty         | true/list    | Registered |
	// | empty         | false        | NOT registered |
	// | includes tool | empty        | Registered |
	// | includes tool | list         | Registered |
	// | includes tool | false        | NOT registered |
	// | excludes tool | any          | NOT registered |
	tests := []struct {
		name         string
		enabledTools []string
		envVarValue  string
		expected     bool
	}{
		{"empty + empty", nil, "", false},
		{"empty + true", nil, "true", true},
		{"empty + channel list", nil, "C123,C456", true},
		{"empty + false", nil, "false", false},
		{"empty + 0", nil, "0", false},
		{"includes + empty", []string{ToolConversationsAddMessage}, "", true},
		{"includes + list", []string{ToolConversationsAddMessage}, "C123", true},
		{"includes + false", []string{ToolConversationsAddMessage}, "false", false},
		{"excludes + empty", []string{ToolConversationsHistory}, "", false},
		{"excludes + true", []string{ToolConversationsHistory}, "true", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolEnabled(t, tt.enabledTools, map[string]string{"SLACK_MCP_ADD_MESSAGE_TOOL": tt.envVarValue}, ToolConversationsAddMessage)
			assert.Equal(t, tt.expected, got)
		})
	}
}
