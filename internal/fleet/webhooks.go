package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/webhooksig"
)

const webhookAttempts = 4

// worker sends queued events until the queue is closed and empty.
func (f *Fleet) worker() {
	defer f.workers.Done()
	for ev := range f.queue {
		f.send(ev)
	}
}

// send POSTs one event, retrying with backoff on network errors, 429 and
// 5xx. Other 4xx mean the receiver rejected it for good, so it gives up.
func (f *Fleet) send(ev event) {
	body, err := json.Marshal(ev)
	if err != nil {
		f.cfg.Logger.Error("marshal webhook", "event_id", ev.ID, "error", err)
		return
	}
	url := f.cfg.WebhookURL + "/webhooks/" + ev.Partner
	backoff := 500 * time.Millisecond

	for attempt := 1; ; attempt++ {
		status, err := f.post(url, body)
		retryable := err != nil || status == http.StatusTooManyRequests || status >= 500
		if err == nil && status < 300 {
			return
		}
		if !retryable || attempt == webhookAttempts {
			f.cfg.Logger.Warn("webhook dropped", "partner", ev.Partner, "event_id", ev.ID,
				"status", status, "error", err, "attempts", attempt)
			return
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-f.sendCtx.Done():
			return
		}
	}
}

func (f *Fleet) post(url string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(f.sendCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Signed per attempt, so a retry carries a fresh timestamp.
	req.Header.Set(webhooksig.Header, webhooksig.Sign(f.cfg.WebhookSecret, body, time.Now()))
	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post webhook: %w", err)
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
