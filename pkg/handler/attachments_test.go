package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bdpdx/slack-mcp-server/pkg/provider"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitIsExternalFile(t *testing.T) {
	assert.False(t, isExternalFile(&slack.File{Mode: "hosted"}))
	assert.True(t, isExternalFile(&slack.File{IsExternal: true}))
	assert.True(t, isExternalFile(&slack.File{ExternalType: "gdrive"}))
	assert.True(t, isExternalFile(&slack.File{Mode: "External"}))
}

func TestUnitAttachmentResultIsValidJSON(t *testing.T) {
	file := &slack.File{ID: "F1", Name: "we\"ird\\name .txt", Mimetype: "text/plain"}
	content := []byte("line1\n\"quoted\" \\ back\x00slash ")

	res, err := attachmentResult(file, content)
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	textContent, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &decoded))
	assert.Equal(t, "F1", decoded["file_id"])
	assert.Equal(t, file.Name, decoded["filename"])
	assert.Equal(t, "none", decoded["encoding"])
	assert.Equal(t, string(content), decoded["content"])
	assert.EqualValues(t, len(content), decoded["size"])

	empty, err := attachmentResult(&slack.File{ID: "F2", Name: "e.txt", Mimetype: "text/plain"}, nil)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(empty.Content[0].(mcp.TextContent).Text), &decoded))
	assert.Equal(t, "", decoded["content"])
}

func TestUnitAttachmentResultBinaryAndImage(t *testing.T) {
	bin := []byte{0x00, 0xff, 0x10}
	res, err := attachmentResult(&slack.File{ID: "F3", Name: "b.bin", Mimetype: "application/octet-stream"}, bin)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &decoded))
	assert.Equal(t, "base64", decoded["encoding"])
	assert.Equal(t, base64.StdEncoding.EncodeToString(bin), decoded["content"])

	img, err := attachmentResult(&slack.File{ID: "F4", Name: "a\"b.png", Mimetype: "image/png"}, bin)
	require.NoError(t, err)
	require.Len(t, img.Content, 2)
	meta, ok := img.Content[0].(mcp.TextContent)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(meta.Text), &decoded))
	assert.Equal(t, "a\"b.png", decoded["filename"])
}

func TestUnitGetBotInfoNeverImpersonates(t *testing.T) {
	userName, realName, ok := getBotInfo("B123", "Alice Smith (CEO)\n‮admin")
	assert.True(t, ok)
	assert.Equal(t, "bot:Alice Smith (CEO) admin (B123)", userName)
	assert.Empty(t, realName, "a bot's chosen username must never become a real name")

	userName, realName, _ = getBotInfo("", "")
	assert.Equal(t, "bot:unknown", userName)
	assert.Empty(t, realName)
}

// fileClient fakes only the download part of provider.SlackAPI.
type fileClient struct {
	provider.SlackAPI
	body string
	err  error
}

func (f fileClient) GetFileContext(_ context.Context, _ string, w io.Writer) error {
	if _, err := io.WriteString(w, f.body); err != nil {
		return err
	}
	return f.err
}

func TestUnitSaveAttachmentWritesToTheFilesFolder(t *testing.T) {
	dir := t.TempDir()
	handler := newPolicyTestHandler(t, nil)
	handler.cfg.FilesDir = dir
	file := &slack.File{ID: "F1", Name: "../report.pdf", Mimetype: "application/pdf"}

	res, err := handler.saveAttachment(t.Context(), fileClient{body: "%PDF-1.7"}, file, "https://files.slack.com/x")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &got))
	path := got["path"].(string)
	assert.Equal(t, filepath.Join(dir, "_report.pdf"), path, "the name is made safe and stays in the folder")
	assert.EqualValues(t, 8, got["size"])
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "%PDF-1.7", string(data))
	assert.NotContains(t, got, "content", "a saved file is not returned inline")

	_, err = handler.saveAttachment(t.Context(), fileClient{body: "part", err: errors.New("network down")}, file, "https://files.slack.com/x")
	require.ErrorContains(t, err, "network down")
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 1, "a failed download leaves nothing behind")

	handler.cfg.FilesDir = ""
	_, err = handler.saveAttachment(t.Context(), fileClient{body: "x"}, file, "https://files.slack.com/x")
	require.ErrorContains(t, err, "files folder is unavailable")
}

func TestUnitCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 4}
	_, err := b.Write([]byte("abcd"))
	require.NoError(t, err)
	_, err = b.Write([]byte("e"))
	assert.ErrorIs(t, err, errInlineTooLarge)
	assert.Equal(t, "abcd", b.String())
}
