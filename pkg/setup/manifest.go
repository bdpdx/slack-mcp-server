package setup

import (
	_ "embed"
	"strings"
)

//go:embed manifest_template.json
var manifestTemplate string

// RenderManifest is the Slack app manifest for a bot named botName (a name
// ValidateBotName accepts, so it needs no JSON escaping).
func RenderManifest(botName string) ([]byte, error) {
	return []byte(strings.ReplaceAll(manifestTemplate, "BOT_NAME", botName)), nil
}
