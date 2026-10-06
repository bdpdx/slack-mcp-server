// Package toolconfig decides which MCP tools are enabled and, for tools that
// write to Slack, which channels they may target. Registration (pkg/server)
// and the handler checks (pkg/handler) both read the same parsed Config, so
// they cannot disagree.
package toolconfig

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Tool names. They mirror the constants in pkg/server, which cannot be
// imported here without a cycle; a server test keeps the two in sync.
const (
	ConversationsAddMessage    = "conversations_add_message"
	ConversationsDeleteMessage = "conversations_delete_message"
	ConversationsOpen          = "conversations_open"
	ConversationsMark          = "conversations_mark"
	ConversationsJoin          = "conversations_join"
	ConversationsLeave         = "conversations_leave"
	ConversationsRename        = "conversations_rename"
	ConversationsCreate        = "conversations_create"
	ConversationsSetTopic      = "conversations_set_topic"
	ConversationsInvite        = "conversations_invite"
	ConversationsInviteShared  = "conversations_invite_shared"
	FilesUpload                = "files_upload"
	ReactionsAdd               = "reactions_add"
	ReactionsRemove            = "reactions_remove"
	AttachmentGetData          = "attachment_get_data"
	UsergroupsCreate           = "usergroups_create"
	UsergroupsUpdate           = "usergroups_update"
	UsergroupsUsersUpdate      = "usergroups_users_update"
	// UsergroupsMe is read-only for action=list but writes for join/leave;
	// the write actions are gated by EnvUsergroupsWriteTool.
	UsergroupsMe = "usergroups_me"
)

// Environment variables that enable gated tools and related behaviour.
const (
	EnvEnabledTools        = "SLACK_MCP_ENABLED_TOOLS"
	EnvAddMessageTool      = "SLACK_MCP_ADD_MESSAGE_TOOL"
	EnvDeleteMessageTool   = "SLACK_MCP_DELETE_MESSAGE_TOOL"
	EnvReactionTool        = "SLACK_MCP_REACTION_TOOL"
	EnvUploadFileTool      = "SLACK_MCP_UPLOAD_FILE_TOOL"
	EnvAttachmentTool      = "SLACK_MCP_ATTACHMENT_TOOL"
	EnvOpenConversation    = "SLACK_MCP_OPEN_CONVERSATION_TOOL"
	EnvMarkTool            = "SLACK_MCP_MARK_TOOL"
	EnvJoinTool            = "SLACK_MCP_JOIN_TOOL"
	EnvRenameChannelTool   = "SLACK_MCP_RENAME_CHANNEL_TOOL"
	EnvCreateChannelTool   = "SLACK_MCP_CREATE_CHANNEL_TOOL"
	EnvSetTopicTool        = "SLACK_MCP_SET_TOPIC_TOOL"
	EnvInviteTool          = "SLACK_MCP_INVITE_TOOL"
	EnvInviteSharedTool    = "SLACK_MCP_INVITE_SHARED_TOOL"
	EnvUsergroupsWriteTool = "SLACK_MCP_USERGROUPS_WRITE_TOOL"
	EnvAllowAsUser         = "SLACK_MCP_ALLOW_AS_USER"
	EnvAddMessageMark      = "SLACK_MCP_ADD_MESSAGE_MARK"
	EnvAddMessageUnfurling = "SLACK_MCP_ADD_MESSAGE_UNFURLING"
)

type kind int

const (
	kindBool kind = iota
	kindChannelList
)

type spec struct {
	env  string
	kind kind
}

// gated lists every tool that is off unless explicitly enabled, with the
// environment variable that enables it. Tools not listed here are read-only
// and are enabled whenever the enabled-tools list allows them.
var gated = map[string]spec{
	ConversationsAddMessage:    {EnvAddMessageTool, kindChannelList},
	ConversationsDeleteMessage: {EnvDeleteMessageTool, kindChannelList},
	ReactionsAdd:               {EnvReactionTool, kindChannelList},
	ReactionsRemove:            {EnvReactionTool, kindChannelList},
	FilesUpload:                {EnvUploadFileTool, kindChannelList},
	AttachmentGetData:          {EnvAttachmentTool, kindBool},
	ConversationsOpen:          {EnvOpenConversation, kindBool},
	ConversationsMark:          {EnvMarkTool, kindBool},
	ConversationsJoin:          {EnvJoinTool, kindBool},
	ConversationsLeave:         {EnvJoinTool, kindBool},
	ConversationsRename:        {EnvRenameChannelTool, kindBool},
	ConversationsCreate:        {EnvCreateChannelTool, kindBool},
	ConversationsSetTopic:      {EnvSetTopicTool, kindBool},
	ConversationsInvite:        {EnvInviteTool, kindBool},
	ConversationsInviteShared:  {EnvInviteSharedTool, kindBool},
	UsergroupsCreate:           {EnvUsergroupsWriteTool, kindBool},
	UsergroupsUpdate:           {EnvUsergroupsWriteTool, kindBool},
	UsergroupsUsersUpdate:      {EnvUsergroupsWriteTool, kindBool},
}

// GatedTools returns the names of all tools that are off unless enabled.
func GatedTools() []string {
	names := make([]string, 0, len(gated))
	for name := range gated {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// EnvVarFor returns the environment variable that enables a gated tool, or
// "" for a read-only tool.
func EnvVarFor(tool string) string {
	return gated[tool].env
}

// ParseBool interprets a boolean setting. true/1/yes/on are on and
// false/0/no/off/"" are off (case-insensitive, trimmed). recognised is false
// for any other value.
func ParseBool(raw string) (value bool, recognised bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, true
	case "", "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

// IsTrue reports whether raw is one of the recognised "on" values.
func IsTrue(raw string) bool {
	v, ok := ParseBool(raw)
	return ok && v
}

// IsExplicitOff reports whether raw is a non-empty "off" value such as false.
func IsExplicitOff(raw string) bool {
	v, ok := ParseBool(raw)
	return ok && !v && strings.TrimSpace(raw) != ""
}

// PolicyMode is how a channel-list setting restricts a tool.
type PolicyMode int

const (
	// PolicyOff means the tool is disabled.
	PolicyOff PolicyMode = iota
	// PolicyAll allows every channel and DM.
	PolicyAll
	// PolicyAllow allows only the listed channels.
	PolicyAllow
	// PolicyDeny allows every channel except the listed ones.
	PolicyDeny
)

// ChannelPolicy is a parsed channel allow/deny list. Entries are
// canonicalised: IDs are trimmed and upper-cased; #channel and @user names
// are trimmed and lower-cased and must be resolved to IDs before matching.
type ChannelPolicy struct {
	Mode    PolicyMode
	Entries []string
}

// Restricted reports whether the policy names specific channels.
func (p ChannelPolicy) Restricted() bool {
	return p.Mode == PolicyAllow || p.Mode == PolicyDeny
}

// CanonicalEntry normalises a single channel reference: names keep their #
// or @ prefix and are lower-cased; anything else is treated as an ID and
// upper-cased.
func CanonicalEntry(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "#") || strings.HasPrefix(s, "@") {
		return strings.ToLower(s)
	}
	return strings.ToUpper(s)
}

// ParseChannelPolicy parses a channel-list setting: a boolean value turns
// the tool off or on for all channels; otherwise a comma-separated list of
// channel IDs or names is an allow-list, or a deny-list when every entry is
// prefixed with "!". Mixing allowed and denied entries is an error.
func ParseChannelPolicy(raw string) (ChannelPolicy, error) {
	if v, ok := ParseBool(raw); ok {
		if v {
			return ChannelPolicy{Mode: PolicyAll}, nil
		}
		return ChannelPolicy{Mode: PolicyOff}, nil
	}

	var allow, deny []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.HasPrefix(item, "!") {
			entry := CanonicalEntry(strings.TrimPrefix(item, "!"))
			if entry == "" || entry == "#" || entry == "@" {
				return ChannelPolicy{}, fmt.Errorf("empty channel after '!' in %q", raw)
			}
			deny = append(deny, entry)
		} else {
			entry := CanonicalEntry(item)
			if entry == "#" || entry == "@" {
				return ChannelPolicy{}, fmt.Errorf("empty channel name in %q", raw)
			}
			allow = append(allow, entry)
		}
	}

	switch {
	case len(allow) > 0 && len(deny) > 0:
		return ChannelPolicy{}, errors.New("cannot mix allowed and disallowed (! prefixed) channels")
	case len(deny) > 0:
		return ChannelPolicy{Mode: PolicyDeny, Entries: deny}, nil
	case len(allow) > 0:
		return ChannelPolicy{Mode: PolicyAllow, Entries: allow}, nil
	}
	return ChannelPolicy{Mode: PolicyOff}, nil
}

// Config is the parsed tool configuration.
type Config struct {
	// enabledTools is the -e / SLACK_MCP_ENABLED_TOOLS list; empty means no
	// restriction beyond each tool's own gate.
	enabledTools map[string]bool
	// raw holds the value of every gating environment variable.
	raw map[string]string
	// policies holds the parsed channel policy for channel-list variables.
	policies map[string]ChannelPolicy

	AllowAsUser    bool
	AddMessageMark bool
	Unfurling      string
}

// Load parses the tool configuration from the enabled-tools list and the
// environment. It fails on a malformed channel list or a boolean setting
// with an unrecognised value.
func Load(enabledTools []string, getenv func(string) string) (*Config, error) {
	c := &Config{
		enabledTools: make(map[string]bool, len(enabledTools)),
		raw:          make(map[string]string),
		policies:     make(map[string]ChannelPolicy),
	}
	for _, t := range enabledTools {
		if t = strings.TrimSpace(t); t != "" {
			c.enabledTools[t] = true
		}
	}

	var errs []error
	for _, s := range gated {
		if _, seen := c.raw[s.env]; seen {
			continue
		}
		value := getenv(s.env)
		c.raw[s.env] = value
		switch s.kind {
		case kindChannelList:
			p, err := ParseChannelPolicy(value)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.env, err))
				continue
			}
			c.policies[s.env] = p
		case kindBool:
			if _, ok := ParseBool(value); !ok {
				errs = append(errs, fmt.Errorf("%s: %q is not a boolean (use true/1/yes/on or false/0/no/off)", s.env, value))
			}
		}
	}

	for _, env := range []string{EnvAllowAsUser, EnvAddMessageMark} {
		if _, ok := ParseBool(getenv(env)); !ok {
			errs = append(errs, fmt.Errorf("%s: %q is not a boolean (use true/1/yes/on or false/0/no/off)", env, getenv(env)))
		}
	}
	c.AllowAsUser = IsTrue(getenv(EnvAllowAsUser))
	c.AddMessageMark = IsTrue(getenv(EnvAddMessageMark))
	c.Unfurling = getenv(EnvAddMessageUnfurling)

	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// MustLoad is Load for tests and callers that have already validated the
// environment; it panics on error.
func MustLoad(enabledTools []string, getenv func(string) string) *Config {
	c, err := Load(enabledTools, getenv)
	if err != nil {
		panic(err)
	}
	return c
}

// FromMap builds a Config from a map of environment values (for tests).
func FromMap(enabledTools []string, env map[string]string) (*Config, error) {
	return Load(enabledTools, func(k string) string { return env[k] })
}

func (c *Config) listed(tool string) bool {
	return c.enabledTools[tool]
}

func (c *Config) hasEnabledList() bool {
	return len(c.enabledTools) > 0
}

// ToolEnabled reports whether a tool should be registered and may run.
//
// A read-only tool is enabled when the enabled-tools list is empty or names
// it. A gated tool is enabled when the enabled-tools list (if any) allows it
// and either its variable turns it on, or its variable is unset and the
// enabled-tools list names it explicitly. An explicit off value (false, 0,
// no, off) always disables it.
func (c *Config) ToolEnabled(tool string) bool {
	if c == nil {
		return false
	}
	s, isGated := gated[tool]
	if !isGated {
		return !c.hasEnabledList() || c.listed(tool)
	}
	if c.hasEnabledList() && !c.listed(tool) {
		return false
	}
	raw := c.raw[s.env]
	if strings.TrimSpace(raw) == "" {
		return c.listed(tool)
	}
	if IsExplicitOff(raw) {
		return false
	}
	if s.kind == kindChannelList {
		return c.policies[s.env].Mode != PolicyOff
	}
	return IsTrue(raw)
}

// ChannelPolicy returns the effective channel policy for a channel-list
// tool. A disabled tool yields PolicyOff; a tool enabled only by the
// enabled-tools list yields PolicyAll.
func (c *Config) ChannelPolicy(tool string) ChannelPolicy {
	if !c.ToolEnabled(tool) {
		return ChannelPolicy{Mode: PolicyOff}
	}
	s := gated[tool]
	if s.kind != kindChannelList {
		return ChannelPolicy{Mode: PolicyAll}
	}
	if strings.TrimSpace(c.raw[s.env]) == "" {
		return ChannelPolicy{Mode: PolicyAll}
	}
	return c.policies[s.env]
}

// DisabledError explains how to enable a disabled tool.
func DisabledError(tool string) error {
	env := EnvVarFor(tool)
	if env == "" {
		return fmt.Errorf("the %s tool is disabled by the enabled-tools list", tool)
	}
	return fmt.Errorf("the %s tool is disabled; set %s=true (or include %s in %s) to enable it", tool, env, tool, EnvEnabledTools)
}

// UsergroupsMeWriteEnabled reports whether usergroups_me may join or leave
// groups. It follows SLACK_MCP_USERGROUPS_WRITE_TOOL, or, when that is unset,
// whether usergroups_users_update is named in the enabled-tools list.
func (c *Config) UsergroupsMeWriteEnabled() bool {
	if c == nil {
		return false
	}
	raw := c.raw[EnvUsergroupsWriteTool]
	if strings.TrimSpace(raw) == "" {
		return c.listed(UsergroupsUsersUpdate)
	}
	return IsTrue(raw)
}
