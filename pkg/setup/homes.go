package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/korotovsky/slack-mcp-server/pkg/agentchat"
)

const (
	TypeClaude = "claude"
	TypeCodex  = "codex"
)

// Home is a candidate agent home.
type Home struct {
	Path   string
	Type   string
	HasEnv bool
}

// EnvPath is the home's env file.
func EnvPath(home string) string { return filepath.Join(home, agentchat.EnvFileName) }

// ExpandPath expands a leading ~ and makes p absolute.
func ExpandPath(p, userHome string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "~":
		p = userHome
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(userHome, p[2:])
	}
	return filepath.Abs(p)
}

// DetectType guesses a home's agent type from its files.
func DetectType(path string) string {
	if _, err := os.Stat(filepath.Join(path, "config.toml")); err == nil {
		return TypeCodex
	}
	if _, err := os.Stat(filepath.Join(path, "settings.json")); err == nil {
		return TypeClaude
	}
	return ""
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// DiscoverHomes lists ~/.claude and ~/.codex when they exist, then saved
// homes, without duplicates.
func DiscoverHomes(userHome string, st *State) []Home {
	var homes []Home
	add := func(path, typ string) {
		for _, h := range homes {
			if h.Path == path {
				return
			}
		}
		homes = append(homes, Home{Path: path, Type: typ, HasEnv: exists(EnvPath(path))})
	}
	for _, std := range []struct{ dir, typ string }{{".claude", TypeClaude}, {".codex", TypeCodex}} {
		if p := filepath.Join(userHome, std.dir); exists(p) {
			add(p, std.typ)
		}
	}
	for _, h := range st.Homes {
		if exists(h.Path) {
			add(h.Path, h.Type)
		}
	}
	return homes
}

// ExistingBin finds the binary path an earlier install used, from the hook
// commands in the homes' settings.json / hooks.json.
func ExistingBin(homes []Home) string {
	for _, h := range homes {
		for _, f := range []string{"settings.json", "hooks.json"} {
			data, err := os.ReadFile(filepath.Join(h.Path, f))
			if err != nil {
				continue
			}
			var doc map[string]any
			if json.Unmarshal(data, &doc) != nil {
				continue
			}
			if bin := binFromHooks(doc["hooks"]); bin != "" {
				return bin
			}
		}
	}
	return ""
}

func binFromHooks(events any) string {
	evs, _ := events.(map[string]any)
	for _, list := range evs {
		entries, _ := list.([]any)
		for _, e := range entries {
			m, _ := e.(map[string]any)
			hs, _ := m["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				if bin, ok := ourBin(cmd); ok {
					return bin
				}
			}
		}
	}
	return ""
}
