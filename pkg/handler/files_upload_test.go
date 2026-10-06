package handler

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/filesdir"
	"github.com/korotovsky/slack-mcp-server/pkg/toolconfig"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func uploadRequest(args map[string]any) mcp.CallToolRequest {
	request := mcp.CallToolRequest{}
	request.Params.Arguments = args
	return request
}

func TestUnitParseParamsToolFilesUpload(t *testing.T) {
	handler := newPolicyTestHandler(t, map[string]string{"SLACK_MCP_UPLOAD_FILE_TOOL": "D123"})

	t.Run("accepts text content and applies filename title default", func(t *testing.T) {
		params, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{
			"channel_id": "D123", "filename": "recap.html", "content": "<h1>Hi</h1>", "thread_ts": "1234567890.123456",
		}))
		require.NoError(t, err)
		require.Equal(t, "D123", params.channel)
		require.Equal(t, "recap.html", params.filename)
		require.Equal(t, "recap.html", params.title)
		require.Equal(t, "<h1>Hi</h1>", params.content)
		require.Equal(t, "1234567890.123456", params.threadTimestamp)
	})

	t.Run("accepts base64 bytes", func(t *testing.T) {
		params, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{
			"channel_id": "D123", "filename": "bytes.bin", "content_base64": base64.StdEncoding.EncodeToString([]byte{0x00, 0xff, 0x41}),
		}))
		require.NoError(t, err)
		require.Equal(t, []byte{0x00, 0xff, 0x41}, []byte(params.content))
	})

	t.Run("enforces exact channel allowlist", func(t *testing.T) {
		request := uploadRequest(map[string]any{"channel_id": "D124", "filename": "recap.txt", "content": "hello"})
		_, err := handler.parseParamsToolFilesUpload(t.Context(), request)
		require.ErrorContains(t, err, "not allowed")
	})

	t.Run("rejects missing and conflicting content", func(t *testing.T) {
		base := map[string]any{"channel_id": "D123", "filename": "recap.txt"}
		_, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(base))
		require.ErrorContains(t, err, "exactly one")

		base["content"] = "text"
		base["content_base64"] = base64.StdEncoding.EncodeToString([]byte("text"))
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(base))
		require.ErrorContains(t, err, "exactly one")
	})

	t.Run("rejects invalid filename, base64, and thread", func(t *testing.T) {
		args := map[string]any{"channel_id": "D123", "filename": "../recap.txt", "content": "hello"}
		_, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(args))
		require.ErrorContains(t, err, "plain filename")

		args["filename"] = "recap.txt"
		delete(args, "content")
		args["content_base64"] = "%%%"
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(args))
		require.ErrorContains(t, err, "invalid")

		args["content_base64"] = base64.StdEncoding.EncodeToString([]byte("hello"))
		args["thread_ts"] = "invalid"
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(args))
		require.ErrorContains(t, err, "thread_ts")
	})

	t.Run("rejects empty and oversized files", func(t *testing.T) {
		args := map[string]any{"channel_id": "D123", "filename": "empty.txt", "content": ""}
		_, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(args))
		require.ErrorContains(t, err, "must not be empty")

		args["filename"] = "large.txt"
		args["content"] = strings.Repeat("a", maxInlineBytes+1)
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(args))
		require.ErrorContains(t, err, "inline content is limited", "large files go through the files folder")

		delete(args, "content")
		args["content_base64"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", maxInlineBytes+1)))
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(args))
		require.ErrorContains(t, err, "inline content is limited")
	})

	t.Run("uploads from the files folder only", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "slack-mcp")
		require.NoError(t, filesdir.Ensure(dir))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("%PDF-1.7"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, "id_ed25519"), []byte("secret"), 0o600))
		handler.cfg.FilesDir = dir
		defer func() { handler.cfg.FilesDir = "" }()

		params, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{"channel_id": "D123", "path": "report.pdf"}))
		require.NoError(t, err)
		defer params.file.Close()
		require.Equal(t, "report.pdf", params.filename, "the filename defaults to the file's name")
		require.Equal(t, 8, params.size)
		require.Empty(t, params.content)

		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{"channel_id": "D123", "path": "../id_ed25519"}))
		require.ErrorContains(t, err, "outside the files folder")
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{"channel_id": "D123", "path": filepath.Join(root, "id_ed25519")}))
		require.ErrorContains(t, err, "outside the files folder")
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{"channel_id": "D123", "path": "report.pdf", "content": "x"}))
		require.ErrorContains(t, err, "exactly one")

		handler.cfg.FilesDir = ""
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{"channel_id": "D123", "path": "report.pdf"}))
		require.ErrorContains(t, err, "files folder is unavailable")
	})

	t.Run("requires upload configuration unless explicitly enabled", func(t *testing.T) {
		handler := newPolicyTestHandler(t, nil)
		_, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{
			"channel_id": "D123", "filename": "recap.html", "content": "ok",
		}))
		require.ErrorContains(t, err, "disabled")

		handler = newPolicyTestHandler(t, map[string]string{"SLACK_MCP_UPLOAD_FILE_TOOL": "false"})
		_, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{
			"channel_id": "D123", "filename": "recap.html", "content": "ok",
		}))
		require.ErrorContains(t, err, "disabled")

		cfg, err := toolconfig.FromMap([]string{"files_upload"}, map[string]string{"SLACK_MCP_ALLOW_AS_USER": "true"})
		require.NoError(t, err)
		handler.cfg = cfg
		params, err := handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{
			"channel_id": "D999", "filename": "recap.html", "content": "ok",
		}))
		require.NoError(t, err)
		require.Equal(t, "D999", params.channel)

		require.False(t, params.asUser)

		params, err = handler.parseParamsToolFilesUpload(t.Context(), uploadRequest(map[string]any{
			"channel_id": "D999", "filename": "recap.html", "content": "ok", "as_user": true,
		}))
		require.NoError(t, err)
		require.True(t, params.asUser)
	})
}
