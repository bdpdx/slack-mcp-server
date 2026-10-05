// Package agentchat delivers Slack channel messages into running Codex and
// Claude Code sessions and provides the `slack-mcp-server chat` helpers.
package agentchat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

// EnvFileName is the configuration file kept in each agent's home directory.
const EnvFileName = "slack-mcp-server.env"

// ResolveEnvFile returns the env file to load: flagValue when given,
// otherwise the file in the detected host's home (Codex when CODEX_THREAD_ID
// is set, Claude Code when CLAUDECODE is set).
func ResolveEnvFile(flagValue string, getenv func(string) string) (string, error) {
	home := getenv("HOME")
	if flagValue != "" {
		return ExpandHome(flagValue, home), nil
	}
	if getenv("CODEX_THREAD_ID") != "" {
		dir := getenv("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		return filepath.Join(ExpandHome(dir, home), EnvFileName), nil
	}
	if getenv("CLAUDECODE") != "" {
		dir := getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			dir = filepath.Join(home, ".claude")
		}
		return filepath.Join(ExpandHome(dir, home), EnvFileName), nil
	}
	return "", fmt.Errorf("no --env-file given and no Codex or Claude Code session detected")
}

// ExpandHome replaces a leading "~" with home.
func ExpandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// LoadEnvFile makes path the only source of SLACK_MCP_* settings: inherited
// SLACK_MCP_* variables are unset, then the file's values are set.
func LoadEnvFile(path string) error {
	values, err := godotenv.Read(path)
	if err != nil {
		return fmt.Errorf("reading env file %s: %w", path, err)
	}
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "SLACK_MCP_") {
			if err := os.Unsetenv(name); err != nil {
				return err
			}
		}
	}
	for k, v := range values {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// Home is one agent's home directory (for example ~/.codex) and the
// slack-agent-chat files kept under it.
type Home struct {
	Dir           string
	EnvFile       string
	StateDir      string
	ControlSocket string
	StateFile     string
	LogFile       string
	CodexSocket   string
}

// NewHome derives a Home from the env file inside it.
func NewHome(envFile string) Home {
	dir := filepath.Dir(envFile)
	state := filepath.Join(dir, "slack-agent-chat")
	return Home{
		Dir:           dir,
		EnvFile:       envFile,
		StateDir:      state,
		ControlSocket: filepath.Join(state, "listener.sock"),
		StateFile:     filepath.Join(state, "state.json"),
		LogFile:       filepath.Join(state, "listener.log"),
		CodexSocket:   filepath.Join(dir, "app-server-control", "app-server-control.sock"),
	}
}
