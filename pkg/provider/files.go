package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MaxFileDownloadBytes caps how many bytes a single file download may read,
// regardless of the size Slack reports for the file.
const MaxFileDownloadBytes = 64 * 1024 * 1024

// ErrFileTooLarge is returned when a download exceeds its byte cap.
var ErrFileTooLarge = errors.New("file exceeds the maximum download size")

// isSlackFileHost reports whether host may receive the bot/user token for a
// file download.
func isSlackFileHost(host string) bool {
	host = strings.ToLower(host)
	switch host {
	case "files.slack.com", "files-edu.slack.com":
		return true
	}
	for _, suffix := range []string{".slack.com", ".slack-edge.com", ".slack-files.com", ".slack-gov.com"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// validateFileURL refuses download URLs that are not https Slack file hosts,
// so the Authorization header is never sent to an attacker-controlled server.
func validateFileURL(rawURL string) (*url.URL, error) {
	if rawURL == "" {
		return nil, errors.New("empty download URL")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("invalid download URL")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-https download URL (scheme %q)", u.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("refusing download URL with embedded credentials")
	}
	if !isSlackFileHost(u.Hostname()) {
		return nil, fmt.Errorf("refusing download from non-Slack host %q", u.Hostname())
	}
	return u, nil
}

// downloadFile fetches a Slack-hosted file with the given token and copies at
// most maxBytes into w. It returns ErrFileTooLarge if the body is longer.
func downloadFile(ctx context.Context, client *http.Client, token, rawURL string, w io.Writer, maxBytes int64) error {
	u, err := validateFileURL(rawURL)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("file download failed: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("%w (%d bytes, limit %d)", ErrFileTooLarge, resp.ContentLength, maxBytes)
	}

	// Read one byte past the cap so an oversized body is detected rather
	// than silently truncated.
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if n > maxBytes {
		return fmt.Errorf("%w (limit %d bytes)", ErrFileTooLarge, maxBytes)
	}
	return nil
}
