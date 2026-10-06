package setup

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/korotovsky/slack-mcp-server/skills"
)

// InstallSkill writes the slack-agent-chat skill files for kind into home,
// with @BIN@ replaced by bin; it returns the files it changed.
func InstallSkill(home, kind, bin string, now time.Time) ([]string, error) {
	files, err := skills.Files(kind)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var changed []string
	for _, n := range names {
		p := filepath.Join(home, "skills", "slack-agent-chat", n)
		wrote, err := replaceFile(p, []byte(strings.ReplaceAll(files[n], "@BIN@", bin)), 0o644, now)
		if err != nil {
			return changed, err
		}
		if wrote {
			changed = append(changed, p)
		}
	}
	return changed, nil
}
