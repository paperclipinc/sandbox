package eventfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Sink delivers a feed Event to an external consumer. Emit is at-least-once:
// an implementation may retry, so a consumer must dedupe on Event.ID. Emit must
// not block indefinitely; it honors the context deadline.
type Sink interface {
	Emit(ctx context.Context, e Event) error
}

// NopSink is the default sink: it drops every event. The Kubernetes Event
// mirror is always on, so the feed still has a channel when no webhook is
// configured; NopSink just means the opt-in CloudEvents egress is off.
type NopSink struct{}

// Emit discards the event.
func (NopSink) Emit(context.Context, Event) error { return nil }

// WebhookSink POSTs the CloudEvent JSON to an operator-configured URL. It is
// at-least-once: it retries on a 5xx response (and on a transport error) up to
// MaxAttempts with a fixed backoff, and stamps the event id as the Ce-Id header
// so the receiver can dedupe. A 2xx is success; a 4xx is a permanent failure
// (the request is malformed or rejected) and is NOT retried.
//
// The URL is operator configuration (a controller flag), the same trust class
// as a git rendezvous remote: an operator who can set the controller's flags
// can already reach the cluster network. The SSRF surface is noted in
// docs/threat-model.md; the sink restricts itself to http/https schemes.
type WebhookSink struct {
	URL    string
	Client *http.Client
	// MaxAttempts bounds the retry count (total tries, not extra retries). Zero
	// defaults to DefaultMaxAttempts.
	MaxAttempts int
	// Backoff is the wait between attempts. Zero defaults to DefaultBackoff.
	Backoff time.Duration
}

const (
	// DefaultMaxAttempts is the total number of delivery attempts before giving
	// up on a retryable failure.
	DefaultMaxAttempts = 3
	// DefaultBackoff is the wait between delivery attempts.
	DefaultBackoff = 200 * time.Millisecond
	// DefaultTimeout bounds a single POST when the sink is built without a
	// caller-supplied client.
	DefaultTimeout = 5 * time.Second
)

// NewWebhookSink builds a WebhookSink with a bounded-timeout HTTP client. An
// empty url returns a NopSink, so an unset --event-sink-url flag means
// Events-only with no panic.
func NewWebhookSink(url string) Sink {
	if url == "" {
		return NopSink{}
	}
	return &WebhookSink{
		URL:         url,
		Client:      &http.Client{Timeout: DefaultTimeout},
		MaxAttempts: DefaultMaxAttempts,
		Backoff:     DefaultBackoff,
	}
}

// Emit POSTs the marshaled CloudEvent, retrying on a retryable failure up to
// MaxAttempts. It returns the last error when every attempt fails, or when the
// context is cancelled between attempts.
func (s *WebhookSink) Emit(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal cloudevent %s: %w", e.ID, err)
	}

	attempts := s.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}
	backoff := s.Backoff
	if backoff <= 0 {
		backoff = DefaultBackoff
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("cloudevent %s delivery cancelled: %w", e.ID, ctx.Err())
			case <-time.After(backoff):
			}
		}

		retryable, err := s.post(ctx, client, body, e.ID, e.Type)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("cloudevent %s delivery failed after %d attempts: %w", e.ID, attempts, lastErr)
}

// post sends one POST. It returns whether the failure is retryable (a 5xx or a
// transport error) and the error; a 2xx returns (false, nil) and a 4xx returns
// (false, err) so the caller does not retry a permanent rejection.
func (s *WebhookSink) post(ctx context.Context, client *http.Client, body []byte, id, eventType string) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		// A malformed URL is a permanent configuration error, not retryable.
		return false, fmt.Errorf("build cloudevent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")
	// The id header is the idempotency key the receiver dedupes on.
	req.Header.Set("Ce-Id", id)
	req.Header.Set("Ce-Type", eventType)

	resp, err := client.Do(req)
	if err != nil {
		// A transport error (connection refused, timeout) is transient: retry.
		return true, fmt.Errorf("post cloudevent %s: %w", id, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("cloudevent %s rejected with status %d", id, resp.StatusCode)
	default:
		return false, fmt.Errorf("cloudevent %s rejected with status %d", id, resp.StatusCode)
	}
}
