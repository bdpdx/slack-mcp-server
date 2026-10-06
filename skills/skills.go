// Package skills carries the slack-agent-chat skill files in the binary, so
// `slack-mcp-server setup` installs the version matching the build.
package skills

import (
	"embed"
	"fmt"
)

//go:embed slack-agent-chat/claude/SKILL.md slack-agent-chat/codex/SKILL.md slack-agent-chat/COLLABORATION.md
var fs embed.FS

// Files returns the skill files for an agent kind ("claude" or "codex"),
// keyed by the name they are installed under. They still contain @BIN@.
func Files(kind string) (map[string]string, error) {
	if kind != "claude" && kind != "codex" {
		return nil, fmt.Errorf("unknown agent kind %q", kind)
	}
	skill, err := fs.ReadFile("slack-agent-chat/" + kind + "/SKILL.md")
	if err != nil {
		return nil, err
	}
	collab, err := fs.ReadFile("slack-agent-chat/COLLABORATION.md")
	if err != nil {
		return nil, err
	}
	return map[string]string{"SKILL.md": string(skill), "COLLABORATION.md": string(collab)}, nil
}
