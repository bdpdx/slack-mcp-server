package handler

import (
	"testing"

	"github.com/bdpdx/slack-mcp-server/pkg/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testChannelsMap() map[string]provider.Channel {
	return map[string]provider.Channel{
		"C001": {ID: "C001", Name: "#general"},
		"C002": {ID: "C002", Name: "#testcase-1", Topic: "first"},
		"C003": {ID: "C003", Name: "#testcase-2", Purpose: "second"},
		"C004": {ID: "C004", Name: "#testcase-3"},
		"G001": {ID: "G001", Name: "#testcase-4", IsPrivate: true},
		"D001": {ID: "D001", Name: "@alice", IsIM: true, User: "U001"},
		"G002": {ID: "G002", Name: "@mpdm-a--b", IsMpIM: true},
	}
}

func channelNames(channels []provider.Channel) []string {
	names := make([]string, 0, len(channels))
	for _, c := range channels {
		names = append(names, c.Name)
	}
	return names
}

func TestUnitFilterChannelsByTypes(t *testing.T) {
	public := filterChannelsByTypes(testChannelsMap(), []string{"public_channel"})
	assert.ElementsMatch(t, []string{"#general", "#testcase-1", "#testcase-2", "#testcase-3"}, channelNames(public))

	private := filterChannelsByTypes(testChannelsMap(), []string{"private_channel"})
	assert.ElementsMatch(t, []string{"#testcase-4"}, channelNames(private))

	dms := filterChannelsByTypes(testChannelsMap(), []string{"im", "mpim"})
	assert.ElementsMatch(t, []string{"@alice", "@mpdm-a--b"}, channelNames(dms))
}

func TestUnitFilterChannelsByQuery(t *testing.T) {
	public := filterChannelsByTypes(testChannelsMap(), []string{"public_channel"})

	byName := filterChannelsByQuery(public, "TESTCASE", map[string]bool{"name": true})
	assert.ElementsMatch(t, []string{"#testcase-1", "#testcase-2", "#testcase-3"}, channelNames(byName))

	byTopic := filterChannelsByQuery(public, "first", map[string]bool{"topic": true})
	assert.Equal(t, []string{"#testcase-1"}, channelNames(byTopic))

	byPurpose := filterChannelsByQuery(public, "second", map[string]bool{"name": true, "purpose": true})
	assert.Equal(t, []string{"#testcase-2"}, channelNames(byPurpose))
}

func TestUnitPaginateChannels(t *testing.T) {
	all := filterChannelsByTypes(testChannelsMap(), []string{"public_channel", "private_channel", "im", "mpim"})
	require.Len(t, all, 7)

	page1, cursor := paginateChannels(all, "", 3)
	require.Len(t, page1, 3)
	require.NotEmpty(t, cursor)

	page2, cursor := paginateChannels(all, cursor, 3)
	require.Len(t, page2, 3)
	require.NotEmpty(t, cursor)

	page3, cursor := paginateChannels(all, cursor, 3)
	require.Len(t, page3, 1)
	require.Empty(t, cursor)

	seen := map[string]bool{}
	for _, c := range append(append(page1, page2...), page3...) {
		require.False(t, seen[c.ID], "duplicate channel %s across pages", c.ID)
		seen[c.ID] = true
	}
}
