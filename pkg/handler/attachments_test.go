package handler

import (
	"encoding/base64"
	"encoding/json"
	"testing"

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
