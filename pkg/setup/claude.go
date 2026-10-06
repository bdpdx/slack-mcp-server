package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Result reports what setup did in one home.
type Result struct {
	Home      string
	Type      string   // TypeClaude or TypeCodex
	Installed bool     // the home was set up completely
	Changed   []string // files written or registrations made
	Manual    []string // commands the user must run
	Notes     []string // anything else to tell the user
}

// readJSONObject reads a JSON object file; a missing file is empty. Invalid
// JSON is an error so the file is never overwritten.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); fix it and run setup again", path, err)
	}
	if doc == nil { // literal null
		doc = map[string]any{}
	}
	return doc, nil
}

func writeJSONObject(path string, doc map[string]any, now time.Time) (bool, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // hook commands keep &&, 2>, < readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil { // Encode ends with a newline
		return false, err
	}
	return replaceFile(path, buf.Bytes(), 0o600, now)
}

// InstallClaude installs the skill, hooks and MCP registration in a Claude
// Code home. The env file is written separately.
func InstallClaude(home, userHome, bin string, r Runner, now time.Time) (Result, error) {
	res := Result{Home: home}
	settings := filepath.Join(home, "settings.json")
	doc, err := readJSONObject(settings)
	if err != nil {
		return res, err
	}
	var events map[string]any
	if raw, ok := doc["hooks"]; ok && raw != nil {
		if events, ok = raw.(map[string]any); !ok {
			return res, fmt.Errorf("%s: \"hooks\" is not a JSON object; fix it and run setup again", settings)
		}
	}
	changed, err := InstallSkill(home, TypeClaude, bin, now)
	res.Changed = append(res.Changed, changed...)
	if err != nil {
		return res, err
	}
	doc["hooks"] = MergeHooks(events, ClaudeHookSpecs(), bin, EnvPath(home), "")
	if wrote, err := writeJSONObject(settings, doc, now); err != nil {
		return res, err
	} else if wrote {
		res.Changed = append(res.Changed, settings)
	}
	manual, err := RegisterMCP(r, TypeClaude, home, userHome, bin)
	if err != nil {
		return res, err
	}
	if manual != "" {
		res.Manual = append(res.Manual, manual)
	} else {
		res.Changed = append(res.Changed, "MCP server registered with claude")
	}
	return res, nil
}
