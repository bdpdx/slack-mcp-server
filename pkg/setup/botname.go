package setup

import "strings"

// ValidateBotName returns why name cannot be a bot name, or "".
func ValidateBotName(name string) string {
	switch {
	case name == "":
		return "The bot name is empty."
	case strings.ContainsAny(name, "_."):
		return "_ and . can't be used in bot names; they are separators in channel names."
	case name == "users":
		return `"users" is reserved for the people-only channel.`
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return "Use lowercase letters, digits and - only."
		}
	}
	return ""
}
