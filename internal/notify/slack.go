package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultSlackTimeout = 10 * time.Second

// slackWebhookPayload is the JSON body expected by a Slack "Incoming
// Webhook" integration. See
// https://api.slack.com/messaging/webhooks#posting_with_webhooks for the
// documented shape; only the commonly used fields are modeled here.
type slackWebhookPayload struct {
	// Text is the plain-text (mrkdwn-formatted) fallback message.
	Text string `json:"text"`

	// Username optionally overrides the webhook's configured bot name.
	Username string `json:"username,omitempty"`

	// IconEmoji optionally overrides the webhook's configured icon, e.g.
	// ":seedling:".
	IconEmoji string `json:"icon_emoji,omitempty"`
}

// SlackNotifier delivers notifications via a Slack incoming webhook URL.
type SlackNotifier struct {
	// WebhookURL is the full incoming-webhook URL, e.g.
	// "https://hooks.slack.com/services/T000/B000/XXXXXXXX". Treat this as
	// a secret: anyone with the URL can post to the configured channel.
	WebhookURL string

	// Username, if set, overrides the webhook's default display name.
	Username string

	// HTTPClient is used to perform the webhook POST. Defaults to a client
	// with a conservative timeout if nil.
	HTTPClient *http.Client
}

// Notify posts message to the configured Slack incoming webhook.
//
// This performs a real outbound HTTP POST when called; it is never invoked
// as part of this project's scaffolding, only wired up for callers such as
// internal/monitor to use once a real webhook URL is configured.
func (s *SlackNotifier) Notify(ctx context.Context, message string) error {
	if s.WebhookURL == "" {
		return fmt.Errorf("notify: SlackNotifier has no WebhookURL configured")
	}

	payload := slackWebhookPayload{
		Text:      message,
		Username:  s.Username,
		IconEmoji: ":seedling:",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: marshaling Slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: building Slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultSlackTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: performing Slack webhook request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("notify: Slack webhook returned status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
