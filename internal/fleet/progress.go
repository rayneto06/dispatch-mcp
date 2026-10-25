package fleet

import "time"

// event is the webhook body sent on every status change.
type event struct {
	ID                string    `json:"id"`
	Type              string    `json:"type"`
	Partner           string    `json:"partner"`
	PartnerDeliveryID string    `json:"partner_delivery_id"`
	Status            string    `json:"status"`
	Sequence          int64     `json:"sequence"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// advance walks one delivery through its statuses on timers. It runs in its
// own goroutine and returns early when the fleet shuts down.
func (f *Fleet) advance(partner, id string) {
	defer f.progress.Done()

	start := time.Now()
	steps := []struct {
		after  time.Duration
		status string
	}{
		{f.cfg.Timings.Assigned, "assigned"},
		{f.cfg.Timings.PickedUp, "picked_up"},
		{f.cfg.Timings.Delivered, "delivered"},
	}
	var prev time.Duration
	for _, s := range steps {
		at := max(f.jitter(s.after), prev) // jitter must not reorder steps
		prev = at

		timer := time.NewTimer(time.Until(start.Add(at)))
		select {
		case <-f.done:
			timer.Stop()
			return
		case <-timer.C:
		}

		ev, ok := f.setStatus(partner, id, s.status)
		if !ok {
			return
		}
		// At-least-once delivery, on purpose: sometimes the same event goes
		// out twice and the receiver has to notice.
		sends := 1
		if f.cfg.Rand() < f.partners[partner].profile.DuplicateRate {
			sends = 2
		}
		for range sends {
			select {
			case f.queue <- ev:
			case <-f.done:
				return
			}
		}
	}
}

func (f *Fleet) jitter(d time.Duration) time.Duration {
	if f.cfg.Jitter == 0 {
		return d
	}
	return time.Duration(float64(d) * (1 + f.cfg.Jitter*(2*f.cfg.Rand()-1)))
}

// setStatus records the change and returns the event to publish.
func (f *Fleet) setStatus(partner, id, status string) (event, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.partners[partner].deliveries[id]
	if !ok {
		return event{}, false
	}
	now := time.Now().UTC()
	d.Status = status
	d.Sequence++
	d.UpdatedAt = now
	return event{
		ID:                "evt_" + randomHex(8),
		Type:              "delivery.status_changed",
		Partner:           partner,
		PartnerDeliveryID: id,
		Status:            status,
		Sequence:          d.Sequence,
		OccurredAt:        now,
	}, true
}
