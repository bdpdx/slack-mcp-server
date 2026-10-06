package setup

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderManifest(t *testing.T) {
	data, err := RenderManifest("team-claude")
	require.NoError(t, err)
	var m struct {
		Display  struct{ Name string } `json:"display_information"`
		Features struct {
			BotUser struct {
				DisplayName string `json:"display_name"`
			} `json:"bot_user"`
		} `json:"features"`
		OAuth struct {
			Scopes struct{ User, Bot []string } `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				BotEvents []string `json:"bot_events"`
			} `json:"event_subscriptions"`
			Interactivity struct {
				IsEnabled bool `json:"is_enabled"`
			} `json:"interactivity"`
			SocketMode bool `json:"socket_mode_enabled"`
		} `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, "team-claude", m.Display.Name)
	assert.Equal(t, "team-claude", m.Features.BotUser.DisplayName)
	assert.Contains(t, m.OAuth.Scopes.User, "groups:write")
	assert.Contains(t, m.OAuth.Scopes.Bot, "im:write")
	assert.Contains(t, m.Settings.Events.BotEvents, "member_joined_channel")
	assert.True(t, m.Settings.Interactivity.IsEnabled)
	assert.True(t, m.Settings.SocketMode)
	assert.NotContains(t, string(data), "BOT_NAME")
}
