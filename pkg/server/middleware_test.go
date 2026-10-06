package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestUnitRedactedToolParams(t *testing.T) {
	request := mcp.CallToolRequest{}
	request.Params.Name = ToolConversationsAddMessage
	request.Params.Arguments = map[string]any{
		"channel_id": "C123",
		"text":       "secret message body",
		"blocks":     []any{map[string]any{"type": "section", "text": "secret block"}},
	}

	logged := redactedToolParams(request)
	assert.Equal(t, "C123", logged["channel_id"])
	assert.Equal(t, "[redacted 19 bytes]", logged["text"])
	assert.NotContains(t, fmt.Sprint(logged), "secret")
}

func TestUnitLoggerMiddlewareRedactsAtInfo(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	mw := buildLoggerMiddleware(zap.New(core))
	handler := mw(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})

	var req mcp.CallToolRequest
	req.Params.Name = ToolConversationsAddMessage
	req.Params.Arguments = map[string]any{"channel_id": "C1", "text": "top secret"}
	_, err := handler(context.Background(), req)
	require.NoError(t, err)

	require.NotEmpty(t, logs.All())
	for _, entry := range logs.All() {
		assert.NotContains(t, fmt.Sprint(entry.ContextMap()), "top secret", entry.Message)
	}
}

func TestUnitAuthRunsBeforeLogger(t *testing.T) {
	t.Setenv("SLACK_MCP_API_KEY", "secret")
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	c := setupMCPClientServer(t,
		toolMiddlewares("sse", logger),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("should not run"), nil
		},
	)

	var callReq mcp.CallToolRequest
	callReq.Params.Name = "test_tool"
	callReq.Params.Arguments = map[string]any{"text": "unauthenticated payload"}
	result, err := c.CallTool(context.Background(), callReq)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Empty(t, logs.FilterMessage("Request received").All(), "unauthenticated calls must not reach the logger")
}
