package toolconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitParseBool(t *testing.T) {
	for _, v := range []string{"true", "1", "yes", "on", " TRUE ", "On", "YES"} {
		got, ok := ParseBool(v)
		assert.True(t, ok, v)
		assert.True(t, got, v)
	}
	for _, v := range []string{"", "false", "0", "no", "off", " FALSE ", "Off"} {
		got, ok := ParseBool(v)
		assert.True(t, ok, v)
		assert.False(t, got, v)
	}
	for _, v := range []string{"C123", "maybe", "2"} {
		_, ok := ParseBool(v)
		assert.False(t, ok, v)
	}
	assert.True(t, IsExplicitOff("false"))
	assert.False(t, IsExplicitOff(""))
}

func TestUnitParseChannelPolicy(t *testing.T) {
	tests := []struct {
		raw     string
		mode    PolicyMode
		entries []string
		wantErr bool
	}{
		{"", PolicyOff, nil, false},
		{"false", PolicyOff, nil, false},
		{"No", PolicyOff, nil, false},
		{"0", PolicyOff, nil, false},
		{"true", PolicyAll, nil, false},
		{"ON", PolicyAll, nil, false},
		{"c123, D456 ,#General", PolicyAllow, []string{"C123", "D456", "#general"}, false},
		{"!c123,!#ops", PolicyDeny, []string{"C123", "#ops"}, false},
		{" , ,", PolicyOff, nil, false},
		{"C123,!C456", PolicyOff, nil, true},
		{"!", PolicyOff, nil, true},
		{"#", PolicyOff, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			p, err := ParseChannelPolicy(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.mode, p.Mode)
			assert.Equal(t, tt.entries, p.Entries)
		})
	}
}

func TestUnitLoadRejectsBadValues(t *testing.T) {
	_, err := FromMap(nil, map[string]string{EnvAddMessageTool: "C1,!C2"})
	require.ErrorContains(t, err, EnvAddMessageTool)
	require.ErrorContains(t, err, "cannot mix")

	_, err = FromMap(nil, map[string]string{EnvJoinTool: "C123"})
	require.ErrorContains(t, err, EnvJoinTool)

	_, err = FromMap(nil, map[string]string{EnvAllowAsUser: "sometimes"})
	require.ErrorContains(t, err, EnvAllowAsUser)
}

func TestUnitToolEnabledAndPolicy(t *testing.T) {
	cfg, err := FromMap(nil, map[string]string{
		EnvAddMessageTool:      "C123",
		EnvReactionTool:        "false",
		EnvJoinTool:            "yes",
		EnvAllowAsUser:         "1",
		EnvAddMessageMark:      "off",
		EnvAddMessageUnfurling: "example.com",
	})
	require.NoError(t, err)

	assert.True(t, cfg.ToolEnabled(ConversationsAddMessage))
	assert.Equal(t, ChannelPolicy{Mode: PolicyAllow, Entries: []string{"C123"}}, cfg.ChannelPolicy(ConversationsAddMessage))
	assert.False(t, cfg.ToolEnabled(ReactionsAdd))
	assert.Equal(t, PolicyOff, cfg.ChannelPolicy(ReactionsAdd).Mode)
	assert.True(t, cfg.ToolEnabled(ConversationsJoin))
	assert.True(t, cfg.ToolEnabled(ConversationsLeave))
	assert.False(t, cfg.ToolEnabled(UsergroupsCreate))
	assert.True(t, cfg.ToolEnabled("conversations_history"), "read-only tools are on")
	assert.True(t, cfg.AllowAsUser)
	assert.False(t, cfg.AddMessageMark)
	assert.Equal(t, "example.com", cfg.Unfurling)

	listed, err := FromMap([]string{ConversationsAddMessage}, nil)
	require.NoError(t, err)
	assert.True(t, listed.ToolEnabled(ConversationsAddMessage))
	assert.Equal(t, PolicyAll, listed.ChannelPolicy(ConversationsAddMessage).Mode)
	assert.False(t, listed.ToolEnabled("conversations_history"))

	var nilCfg *Config
	assert.False(t, nilCfg.ToolEnabled("conversations_history"))
}
