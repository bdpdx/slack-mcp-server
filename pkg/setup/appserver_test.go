package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The rest of this package's tests expect systemd, whatever machine runs them;
// the launchd tests switch with useLaunchd.
func init() { hostOS = "linux" }

func useLaunchd(t *testing.T) {
	t.Cleanup(func() { hostOS = "linux" })
	hostOS = "darwin"
}

func TestAppServerLabel(t *testing.T) {
	assert.Equal(t, "com.openai.codex-test.app-server", appServerLabel("/u/.codex-test"))
	assert.Equal(t, "com.openai.codex.app-server", appServerLabel("/u/.codex"))
	assert.Equal(t, "com.openai.codex-x.app-server", appServerLabel("/u/codex-x"))
}
