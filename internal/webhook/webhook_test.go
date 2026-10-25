package webhook

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/webhooksig"
)

var (
	secret = []byte("s3cret")
	t0     = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
)

type fixture struct {
	mux   *http.ServeMux
	store *dispatch.Store
	reg   *health.Registry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: dispatch.NewStore(dispatch.DefaultLimits()), reg: health.NewRegistry([]string{"alpha", "beta"}, t0)}
	f.store.AddDelivery(dispatch.Delivery{
		ID: "d_1", QuoteID: "q_1", Partner: "alpha", PartnerDeliveryID: "alpha_1",
		Status: dispatch.StatusPending, Sequence: 1, CreatedAt: t0,
		History: []dispatch.HistoryEntry{{Status: dispatch.StatusPending, At: t0, Source: dispatch.SourceCreate}},
	})
	h := &Handler{Secret: secret, Store: f.store, Health: f.reg, Now: func() time.Time { return t0 }, Tolerance: 5 * time.Minute,
		Logger: slog.New(slog.DiscardHandler)}
	f.mux = http.NewServeMux()
	f.mux.Handle("POST /webhooks/{partner}", h)
	return f
}

func eventJSON(id, partner, deliveryID, status string, seq int) string {
	return fmt.Sprintf(`{"id":%q,"type":"delivery.status_changed","partner":%q,"partner_delivery_id":%q,"status":%q,"sequence":%d,"occurred_at":"2026-10-25T12:01:00Z"}`,
		id, partner, deliveryID, status, seq)
}

func (f *fixture) post(path, body string, signedAt time.Time, sign bool) int {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	if sign {
		req.Header.Set(webhooksig.Header, webhooksig.Sign(secret, []byte(body), signedAt))
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestWebhook(t *testing.T) {
	f := newFixture(t)
	alpha := f.reg.Partner("alpha")
	big := `{"id":"evt_big","pad":"` + strings.Repeat("x", 65<<10) + `"}`

	steps := []struct {
		name       string
		path, body string
		signedAt   time.Time
		sign       bool
		wantCode   int
		counter    func() int64
		wantDelta  int64
		wantStatus dispatch.Status
	}{
		{"valid assigned", "/webhooks/alpha", eventJSON("evt_1", "alpha", "alpha_1", "assigned", 2), t0, true, 200,
			alpha.WebhooksReceived.Load, 1, dispatch.StatusAssigned},
		{"same event again", "/webhooks/alpha", eventJSON("evt_1", "alpha", "alpha_1", "assigned", 2), t0, true, 200,
			alpha.DuplicatesDropped.Load, 1, dispatch.StatusAssigned},
		{"older sequence", "/webhooks/alpha", eventJSON("evt_0", "alpha", "alpha_1", "pending", 1), t0, true, 200,
			alpha.OutOfOrderDropped.Load, 1, dispatch.StatusAssigned},
		{"bad signature", "/webhooks/alpha", eventJSON("evt_2", "alpha", "alpha_1", "delivered", 4), t0, false, 401,
			alpha.BadSignatures.Load, 1, dispatch.StatusAssigned},
		{"signature 10 min old", "/webhooks/alpha", eventJSON("evt_3", "alpha", "alpha_1", "delivered", 4), t0.Add(-10 * time.Minute), true, 401,
			alpha.BadSignatures.Load, 1, dispatch.StatusAssigned},
		{"body over 64 KiB", "/webhooks/alpha", big, t0, true, 413, alpha.WebhooksReceived.Load, 0, dispatch.StatusAssigned},
		{"invalid JSON, signed", "/webhooks/alpha", `{not json`, t0, true, 400, alpha.WebhooksReceived.Load, 0, dispatch.StatusAssigned},
		{"missing fields, signed", "/webhooks/alpha", `{"id":"evt_9"}`, t0, true, 400, alpha.WebhooksReceived.Load, 0, dispatch.StatusAssigned},
		{"unknown partner path", "/webhooks/delta", eventJSON("evt_4", "delta", "x", "assigned", 2), t0, true, 404,
			alpha.WebhooksReceived.Load, 0, dispatch.StatusAssigned},
		{"unknown status, signed", "/webhooks/alpha", eventJSON("evt_8", "alpha", "alpha_1", "teleported", 9), t0, true, 400,
			alpha.WebhooksReceived.Load, 0, dispatch.StatusAssigned},
		{"partner mismatch", "/webhooks/alpha", eventJSON("evt_5", "beta", "alpha_1", "delivered", 4), t0, true, 400,
			alpha.WebhooksReceived.Load, 0, dispatch.StatusAssigned},
		{"valid delivered", "/webhooks/alpha", eventJSON("evt_6", "alpha", "alpha_1", "delivered", 4), t0, true, 200,
			alpha.WebhooksReceived.Load, 1, dispatch.StatusDelivered},
	}
	for _, st := range steps {
		before := st.counter()
		if code := f.post(st.path, st.body, st.signedAt, st.sign); code != st.wantCode {
			t.Errorf("%s: status = %d, want %d", st.name, code, st.wantCode)
		}
		if delta := st.counter() - before; delta != st.wantDelta {
			t.Errorf("%s: counter delta = %d, want %d", st.name, delta, st.wantDelta)
		}
		if d, _ := f.store.Delivery("d_1"); d.Status != st.wantStatus {
			t.Errorf("%s: delivery status = %s, want %s", st.name, d.Status, st.wantStatus)
		}
	}

	d, _ := f.store.Delivery("d_1")
	if len(d.History) != 3 || d.LastUpdateSource != dispatch.SourceWebhook {
		t.Errorf("history = %+v, want create, assigned, delivered", d.History)
	}
}

func TestWebhookUnknownDelivery(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/webhooks/alpha", eventJSON("evt_7", "alpha", "alpha_999", "assigned", 2), t0, true); code != 200 {
		t.Errorf("status = %d, want 200 so the partner stops retrying", code)
	}
	if n := f.reg.Partner("alpha").UnknownDelivery.Load(); n != 1 {
		t.Errorf("unknown_delivery_events = %d, want 1", n)
	}
}
