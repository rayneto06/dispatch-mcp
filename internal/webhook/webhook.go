// Package webhook receives partner status events. Partners deliver at least
// once, so the same event can arrive twice, late, or after a newer one; the
// handler makes all of those harmless.
package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/webhooksig"
)

// MaxBodyBytes caps a webhook body; events are a few hundred bytes.
const MaxBodyBytes = 64 << 10

// Handler serves POST /webhooks/{partner}.
type Handler struct {
	Secret    []byte
	Store     *dispatch.Store
	Health    *health.Registry
	Now       func() time.Time
	Tolerance time.Duration // accepted clock skew / replay window for signatures
	Logger    *slog.Logger
}

type event struct {
	ID                string    `json:"id"`
	Type              string    `json:"type"`
	Partner           string    `json:"partner"`
	PartnerDeliveryID string    `json:"partner_delivery_id"`
	Status            string    `json:"status"`
	Sequence          int64     `json:"sequence"`
	OccurredAt        time.Time `json:"occurred_at"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("partner")
	c := h.Health.Partner(name)
	if c == nil {
		http.Error(w, "unknown partner", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	// Verify the raw bytes before parsing anything: unsigned input never
	// reaches the JSON decoder or the store.
	if err := webhooksig.Verify(h.Secret, r.Header.Get(webhooksig.Header), body, h.Now(), h.Tolerance); err != nil {
		c.BadSignatures.Add(1)
		h.Logger.Warn("webhook rejected", "partner", name, "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var ev event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.PartnerDeliveryID == "" || ev.Sequence <= 0 {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	status, ok := dispatch.ParseStatus(ev.Status)
	if !ok {
		http.Error(w, "unknown status", http.StatusBadRequest)
		return
	}
	if ev.Partner != name {
		http.Error(w, "event partner does not match the URL", http.StatusBadRequest)
		return
	}
	c.WebhooksReceived.Add(1)

	outcome := "applied"
	switch {
	case h.Store.MarkEvent(ev.ID, h.Now()):
		c.DuplicatesDropped.Add(1)
		outcome = "duplicate"
	default:
		switch h.Store.ApplyStatus(dispatch.StatusUpdate{
			Partner:           name,
			PartnerDeliveryID: ev.PartnerDeliveryID,
			Status:            status,
			Sequence:          ev.Sequence,
			At:                ev.OccurredAt,
			Source:            dispatch.SourceWebhook,
		}, h.Now()) {
		case dispatch.Stale:
			c.OutOfOrderDropped.Add(1)
			outcome = "out_of_order"
		case dispatch.Unknown:
			// Ack anyway. The store holds the update until the booking it
			// belongs to is recorded, and get_delivery's polling covers the rest.
			c.UnknownDelivery.Add(1)
			outcome = "parked"
		}
	}
	h.Logger.Info("webhook", "partner", name, "event_id", ev.ID, "status", ev.Status, "sequence", ev.Sequence, "outcome", outcome)
	w.WriteHeader(http.StatusOK)
}
