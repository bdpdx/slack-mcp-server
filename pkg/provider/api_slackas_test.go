package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitSlackAs(t *testing.T) {
	bot := &MCPSlackClient{isBotToken: true}
	user := &MCPSlackClient{}

	t.Run("bot and user tokens", func(t *testing.T) {
		ap := &ApiProvider{client: bot, userClient: user}
		assert.True(t, ap.HasUserClient())

		client, err := ap.SlackAs(false)
		require.NoError(t, err)
		assert.Same(t, bot, client)

		client, err = ap.SlackAs(true)
		require.NoError(t, err)
		assert.Same(t, user, client)
	})

	t.Run("bot token only rejects as_user", func(t *testing.T) {
		ap := &ApiProvider{client: bot}

		client, err := ap.SlackAs(false)
		require.NoError(t, err)
		assert.Same(t, bot, client)

		_, err = ap.SlackAs(true)
		require.ErrorContains(t, err, "as_user requires a user token")
	})

	t.Run("user token only", func(t *testing.T) {
		ap := &ApiProvider{client: user}
		assert.False(t, ap.HasUserClient())

		client, err := ap.SlackAs(true)
		require.NoError(t, err)
		assert.Same(t, user, client)
	})
}
