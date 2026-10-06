package text

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/toolconfig"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
	"golang.org/x/net/publicsuffix"
)

func AttachmentToText(att slack.Attachment) string {
	var parts []string

	if att.Title != "" {
		if att.TitleLink != "" {
			parts = append(parts, fmt.Sprintf("Title: [%s](%s)", att.Title, att.TitleLink))
		} else {
			parts = append(parts, fmt.Sprintf("Title: %s", att.Title))
		}
	}

	if att.AuthorName != "" {
		parts = append(parts, fmt.Sprintf("Author: %s", att.AuthorName))
	}

	if att.Pretext != "" {
		parts = append(parts, fmt.Sprintf("Pretext: %s", att.Pretext))
	}

	if att.Text != "" {
		parts = append(parts, fmt.Sprintf("Text: %s", att.Text))
	}

	for _, f := range att.Fields {
		if f.Title != "" && f.Value != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", f.Title, f.Value))
		} else if f.Title != "" {
			parts = append(parts, f.Title)
		} else if f.Value != "" {
			parts = append(parts, f.Value)
		}
	}

	if att.Footer != "" {
		ts, _ := TimestampToIsoRFC3339(string(att.Ts) + ".000000")

		parts = append(parts, fmt.Sprintf("Footer: %s @ %s", att.Footer, ts))
	}

	if blocksText := BlocksToText(att.Blocks); blocksText != "" {
		parts = append(parts, fmt.Sprintf("Blocks: %s", blocksText))
	}

	result := strings.Join(parts, "; ")

	result = strings.ReplaceAll(result, "\n", " ")
	result = strings.ReplaceAll(result, "\r", " ")
	result = strings.ReplaceAll(result, "\t", " ")
	result = strings.TrimSpace(result)

	return result
}

// BlocksToText extracts text content from Slack Block Kit structures.
func BlocksToText(blocks slack.Blocks) string {
	if len(blocks.BlockSet) == 0 {
		return ""
	}

	var parts []string

	for _, block := range blocks.BlockSet {
		switch b := block.(type) {
		case *slack.HeaderBlock:
			if b.Text != nil && b.Text.Text != "" {
				parts = append(parts, b.Text.Text)
			}
		case *slack.SectionBlock:
			if b.Text != nil && b.Text.Text != "" {
				parts = append(parts, b.Text.Text)
			}
			for _, field := range b.Fields {
				if field != nil && field.Text != "" {
					parts = append(parts, field.Text)
				}
			}
		case *slack.RichTextBlock:
			if t := richTextBlockToText(b); t != "" {
				parts = append(parts, t)
			}
		case *slack.ContextBlock:
			for _, elem := range b.ContextElements.Elements {
				if txt, ok := elem.(*slack.TextBlockObject); ok && txt != nil && txt.Text != "" {
					parts = append(parts, txt.Text)
				}
			}
		}
	}

	return strings.Join(parts, " ")
}

// FilesToText extracts text metadata from email file attachments.
// Separators are chosen so the metadata survives the text-processing pipeline.
func FilesToText(files []slack.File) string {
	var parts []string

	for _, f := range files {
		if f.Filetype != "email" && f.Mode != "email" {
			continue
		}

		var emailParts []string

		if len(f.From) > 0 {
			if s := formatEmailUser(f.From[0]); s != "" {
				emailParts = append(emailParts, "From: "+s)
			}
		}

		if len(f.Cc) > 0 {
			var ccParts []string
			for _, c := range f.Cc {
				if s := formatEmailUser(c); s != "" {
					ccParts = append(ccParts, s)
				}
			}
			if len(ccParts) > 0 {
				emailParts = append(emailParts, "CC: "+strings.Join(ccParts, "/"))
			}
		}

		if f.Subject != "" {
			emailParts = append(emailParts, fmt.Sprintf("Subject: %s", f.Subject))
		} else if f.Title != "" {
			emailParts = append(emailParts, fmt.Sprintf("Subject: %s", f.Title))
		}

		if len(emailParts) > 0 {
			parts = append(parts, "Email, "+strings.Join(emailParts, ", "))
		}
	}

	return strings.Join(parts, " ")
}

func richTextBlockToText(rtb *slack.RichTextBlock) string {
	var parts []string

	for _, elem := range rtb.Elements {
		if t := richTextElementToText(elem); t != "" {
			parts = append(parts, t)
		}
	}

	return strings.Join(parts, " ")
}

func richTextElementToText(elem slack.RichTextElement) string {
	switch e := elem.(type) {
	case *slack.RichTextSection:
		return richTextSectionToText(e)
	case *slack.RichTextList:
		var parts []string
		for _, listElem := range e.Elements {
			if t := richTextElementToText(listElem); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, " ")
	case *slack.RichTextQuote:
		return richTextSectionToText((*slack.RichTextSection)(e))
	case *slack.RichTextPreformatted:
		return richTextSectionToText(&e.RichTextSection)
	}
	return ""
}

func formatEmailUser(u slack.EmailFileUserInfo) string {
	addr := strings.ReplaceAll(u.Address, "@", " at ")
	if u.Name != "" && addr != "" {
		return u.Name + " - " + addr
	} else if u.Name != "" {
		return u.Name
	} else if addr != "" {
		return addr
	}
	return ""
}

func richTextSectionToText(section *slack.RichTextSection) string {
	var parts []string

	for _, elem := range section.Elements {
		switch e := elem.(type) {
		case *slack.RichTextSectionTextElement:
			if e.Text != "" {
				parts = append(parts, e.Text)
			}
		case *slack.RichTextSectionLinkElement:
			if e.Text != "" {
				parts = append(parts, e.Text)
			} else if e.URL != "" {
				parts = append(parts, e.URL)
			}
		case *slack.RichTextSectionBroadcastElement:
			if e.Range != "" {
				parts = append(parts, "@"+e.Range)
			}
		}
	}

	return strings.Join(parts, "")
}

func AttachmentsTo2CSV(msgText string, attachments []slack.Attachment) string {
	if len(attachments) == 0 {
		return ""
	}

	var descriptions []string
	for _, att := range attachments {
		plainText := AttachmentToText(att)
		if plainText != "" {
			descriptions = append(descriptions, fmt.Sprintf("%s", plainText))
		}
	}

	prefix := ""
	if msgText != "" {
		prefix = ". "
	}

	return prefix + strings.Join(descriptions, ", ")
}

var (
	// unfurlURLRegex stops at characters that delimit a URL in Slack mrkdwn
	// (<url|text>) and in JSON-encoded blocks.
	unfurlURLRegex    = regexp.MustCompile(`https?://[^\s<>|"\\]+`)
	unfurlDomainRegex = regexp.MustCompile(`\b(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}\b`)
)

// IsUnfurlingEnabled reports whether link unfurling may be enabled for a
// message. opt is SLACK_MCP_ADD_MESSAGE_UNFURLING: a boolean turns unfurling
// off or on for every link; otherwise it is a comma-separated domain
// allow-list and every URL and bare domain in text must be on it. A URL that
// cannot be parsed is treated as not allowed.
func IsUnfurlingEnabled(text string, opt string, logger *zap.Logger) bool {
	if v, ok := toolconfig.ParseBool(opt); ok {
		return v
	}
	return unfurlTextAllowed(text, parseUnfurlAllowList(opt), opt, logger)
}

// BlocksUnfurlAllowed applies the same domain allow-list to every URL that
// appears anywhere in a message's Block Kit blocks (link elements, image and
// button URLs, mrkdwn text). It returns true when there are no blocks or when
// opt is a boolean, in which case IsUnfurlingEnabled alone decides.
func BlocksUnfurlAllowed(blocks []slack.Block, opt string, logger *zap.Logger) bool {
	if len(blocks) == 0 {
		return true
	}
	if _, ok := toolconfig.ParseBool(opt); ok {
		return true
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return false
	}
	// Undo JSON's HTML escaping so < > & do not hide URL boundaries.
	encoded := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(raw))
	return unfurlTextAllowed(encoded, parseUnfurlAllowList(opt), opt, logger)
}

func parseUnfurlAllowList(opt string) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, d := range strings.Split(opt, ",") {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		allowed[d] = struct{}{}
	}
	return allowed
}

func unfurlTextAllowed(text string, allowed map[string]struct{}, opt string, logger *zap.Logger) bool {
	deny := func(host string) bool {
		if logger != nil {
			logger.Warn("Security: attempt to unfurl non-whitelisted host",
				zap.String("host", host),
				zap.String("allowed", opt),
			)
		}
		return false
	}

	for _, rawURL := range unfurlURLRegex.FindAllString(text, -1) {
		u, err := url.Parse(rawURL)
		if err != nil || u.Hostname() == "" {
			return deny(rawURL)
		}
		host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		if _, ok := allowed[host]; !ok {
			return deny(host)
		}
	}

	txtNoURLs := unfurlURLRegex.ReplaceAllString(text, " ")
	for _, d := range unfurlDomainRegex.FindAllString(txtNoURLs, -1) {
		d = strings.ToLower(d)

		if _, icann := publicsuffix.PublicSuffix(d); !icann {
			continue
		}

		if _, ok := allowed[d]; !ok {
			return deny(d)
		}
	}

	return true
}

func Workspace(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	parts := strings.Split(host, ".")
	if len(parts) < 3 {
		return "", fmt.Errorf("invalid Slack URL: %q", rawURL)
	}
	return parts[0], nil
}

func TimestampToIsoRFC3339(slackTS string) (string, error) {
	parts := strings.Split(slackTS, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid slack timestamp format: %s", slackTS)
	}

	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return "", fmt.Errorf("failed to parse seconds: %v", err)
	}

	microseconds, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("failed to parse microseconds: %v", err)
	}

	t := time.Unix(seconds, microseconds*1000)

	return t.UTC().Format(time.RFC3339), nil
}

func ProcessText(s string) string {
	s = normalizeLinks(s)
	s = stripUnsafeRunes(s)
	s = collapseInlineSpaces(s)

	return strings.TrimSpace(s)
}

var (
	slackLinkRegex    = regexp.MustCompile(`<(https?://[^>|]+)\|([^>]+)>`)
	markdownLinkRegex = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)]+)\)`)
	htmlLinkRegex     = regexp.MustCompile(`<a\s+href=["']([^"']+)["'][^>]*>([^<]+)</a>`)
	inlineSpaceRegex  = regexp.MustCompile(`[ \t]+`)
)

func normalizeLinks(text string) string {
	isLastInText := func(original string, currentText string) bool {
		linkPos := strings.LastIndex(currentText, original)
		if linkPos == -1 {
			return false
		}
		afterLink := strings.TrimSpace(currentText[linkPos+len(original):])
		return afterLink == ""
	}

	render := func(url, linkText string, isLast bool) string {
		out := url + " - " + linkText
		if !isLast {
			out += ","
		}
		return out
	}

	for _, match := range slackLinkRegex.FindAllStringSubmatch(text, -1) {
		original := match[0]
		text = strings.Replace(text, original, render(match[1], match[2], isLastInText(original, text)), 1)
	}

	for _, match := range markdownLinkRegex.FindAllStringSubmatch(text, -1) {
		original := match[0]
		text = strings.Replace(text, original, render(match[2], match[1], isLastInText(original, text)), 1)
	}

	for _, match := range htmlLinkRegex.FindAllStringSubmatch(text, -1) {
		original := match[0]
		text = strings.Replace(text, original, render(match[1], match[2], isLastInText(original, text)), 1)
	}

	return text
}

// stripUnsafeRunes removes runes that are display-corrupting or carry no
// semantic content: C0/C1 controls (except \t \n \r), DEL, BOM, ZWSP,
// LRM/RLM, bidi overrides, and bidi isolates. Bidi overrides are a known
// prompt-injection vector in chat corpora. U+200C (ZWNJ) and U+200D (ZWJ)
// are preserved: they are required for Persian and Arabic letter joining
// and for emoji ZWJ sequences such as family and flag emoji.
func stripUnsafeRunes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7F:
			continue
		case r >= 0x80 && r <= 0x9F:
			continue
		case r == 0xFEFF:
			continue
		case r == 0x200B, r == 0x200E, r == 0x200F: // ZWSP, LRM, RLM; U+200C ZWNJ and U+200D ZWJ preserved
			continue
		case r >= 0x202A && r <= 0x202E:
			continue
		case r >= 0x2066 && r <= 0x2069:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func collapseInlineSpaces(s string) string {
	return inlineSpaceRegex.ReplaceAllString(s, " ")
}
