package dispatch

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func newDelivery(i int, created time.Time) Delivery {
	return Delivery{
		ID:                fmt.Sprintf("d_%d", i),
		QuoteID:           fmt.Sprintf("q_%d", i),
		Partner:           "alpha",
		PartnerDeliveryID: fmt.Sprintf("alpha_%d", i),
		Status:            StatusPending,
		Sequence:          1,
		CreatedAt:         created,
		UpdatedAt:         created,
		LastUpdateSource:  SourceCreate,
		History:           []HistoryEntry{{Status: StatusPending, At: created, Source: SourceCreate}},
	}
}

func TestQuoteLifecycle(t *testing.T) {
	s := NewStore(DefaultLimits())
	if err := s.PutQuotes([]Quote{{ID: "q_1", Partner: "alpha", ExpiresAt: t0.Add(10 * time.Minute)}}, t0); err != nil {
		t.Fatal(err)
	}
	if q, err := s.Quote("q_1", t0.Add(time.Minute)); err != nil || q.Partner != "alpha" {
		t.Errorf("Quote = %+v, %v", q, err)
	}
	if _, err := s.Quote("q_1", t0.Add(11*time.Minute)); !errors.Is(err, ErrQuoteExpired) {
		t.Errorf("expired quote err = %v", err)
	}
	if _, err := s.Quote("q_nope", t0); !errors.Is(err, ErrQuoteNotFound) {
		t.Errorf("unknown quote err = %v", err)
	}
}

func TestPutQuotesPrunesAndCaps(t *testing.T) {
	s := NewStore(Limits{MaxDeliveries: 10, MaxQuotes: 2, MaxSeenEvents: 10, SeenEventTTL: time.Hour})
	exp := t0.Add(time.Minute)
	if err := s.PutQuotes([]Quote{{ID: "q_1", ExpiresAt: exp}, {ID: "q_2", ExpiresAt: exp}}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutQuotes([]Quote{{ID: "q_3", ExpiresAt: exp}}, t0); !errors.Is(err, ErrCapacity) {
		t.Errorf("over cap err = %v, want ErrCapacity", err)
	}
	// Once the first two expire they are pruned and there is room again.
	if err := s.PutQuotes([]Quote{{ID: "q_3", ExpiresAt: t0.Add(time.Hour)}}, t0.Add(2*time.Minute)); err != nil {
		t.Errorf("after expiry err = %v", err)
	}
}

func TestAddDeliveryIdempotentByQuote(t *testing.T) {
	s := NewStore(Limits{MaxDeliveries: 2, MaxQuotes: 10, MaxSeenEvents: 10, SeenEventTTL: time.Hour})
	first, existed, err := s.AddDelivery(newDelivery(1, t0))
	if err != nil || existed {
		t.Fatalf("first add = %v existed=%v", err, existed)
	}
	again := newDelivery(1, t0)
	again.ID = "d_other"
	got, existed, err := s.AddDelivery(again)
	if err != nil || !existed || got.ID != first.ID {
		t.Errorf("same quote again = %s existed=%v err=%v, want %s existed=true", got.ID, existed, err, first.ID)
	}
	s.AddDelivery(newDelivery(2, t0))
	if _, _, err := s.AddDelivery(newDelivery(3, t0)); !errors.Is(err, ErrCapacity) {
		t.Errorf("third distinct add err = %v, want ErrCapacity", err)
	}
	if _, err := s.Delivery("d_nope"); !errors.Is(err, ErrDeliveryNotFound) {
		t.Errorf("unknown delivery err = %v", err)
	}
}

func ids(ds []Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func TestListCursorPagination(t *testing.T) {
	s := NewStore(DefaultLimits())
	for i := 1; i <= 5; i++ {
		s.AddDelivery(newDelivery(i, t0.Add(time.Duration(i)*time.Second)))
	}

	page, cur, err := s.List("", 2)
	if err != nil || fmt.Sprint(ids(page)) != "[d_5 d_4]" || cur == "" {
		t.Fatalf("page 1 = %v %q %v", ids(page), cur, err)
	}
	if page[0].History != nil {
		t.Errorf("list items should omit history")
	}
	page2, cur2, _ := s.List(cur, 2)
	if fmt.Sprint(ids(page2)) != "[d_3 d_2]" {
		t.Errorf("page 2 = %v", ids(page2))
	}
	page3, cur3, _ := s.List(cur2, 2)
	if fmt.Sprint(ids(page3)) != "[d_1]" || cur3 != "" {
		t.Errorf("page 3 = %v %q, want [d_1] and no cursor", ids(page3), cur3)
	}

	// A delivery created between pages must not shift the pages after the cursor.
	s.AddDelivery(newDelivery(6, t0.Add(6*time.Second)))
	if again, _, _ := s.List(cur, 2); fmt.Sprint(ids(again)) != "[d_3 d_2]" {
		t.Errorf("page 2 after insert = %v, want [d_3 d_2]", ids(again))
	}

	for _, bad := range []string{"%%%", "bm9waXBl", "eHx5"} { // garbage, "nopipe", "x|y"
		if _, _, err := s.List(bad, 2); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q err = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

func TestListTiesOnCreatedAt(t *testing.T) {
	s := NewStore(DefaultLimits())
	for i := 1; i <= 3; i++ {
		s.AddDelivery(newDelivery(i, t0)) // same timestamp: ID breaks the tie
	}
	page, cur, _ := s.List("", 2)
	rest, _, _ := s.List(cur, 2)
	if got := fmt.Sprint(ids(page), ids(rest)); got != "[d_3 d_2] [d_1]" {
		t.Errorf("pages = %s", got)
	}
}

func TestApplyStatusOrdering(t *testing.T) {
	s := NewStore(DefaultLimits())
	s.AddDelivery(newDelivery(1, t0))
	upd := func(status Status, seq int64, src Source) StatusUpdate {
		return StatusUpdate{Partner: "alpha", PartnerDeliveryID: "alpha_1", Status: status, Sequence: seq, At: t0.Add(time.Duration(seq) * time.Minute), Source: src}
	}

	steps := []struct {
		u    StatusUpdate
		want ApplyResult
	}{
		{upd(StatusAssigned, 2, SourceWebhook), Applied},
		{upd(StatusAssigned, 2, SourceWebhook), Stale},
		{upd(StatusPending, 1, SourceWebhook), Stale},
		{upd(StatusDelivered, 4, SourceWebhook), Applied}, // gaps are fine
		{StatusUpdate{Partner: "alpha", PartnerDeliveryID: "alpha_999", Status: StatusAssigned, Sequence: 2}, Unknown},
		{StatusUpdate{Partner: "beta", PartnerDeliveryID: "alpha_1", Status: StatusAssigned, Sequence: 9}, Unknown},
	}
	for i, st := range steps {
		if got := s.ApplyStatus(st.u, t0.Add(time.Hour)); got != st.want {
			t.Errorf("step %d: ApplyStatus = %v, want %v", i, got, st.want)
		}
	}

	d, _ := s.Delivery("d_1")
	if d.Status != StatusDelivered || d.Sequence != 4 || d.LastUpdateSource != SourceWebhook || !d.SyncedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("delivery = %+v", d)
	}
	var got []string
	for _, h := range d.History {
		got = append(got, string(h.Source)+":"+string(h.Status))
	}
	if fmt.Sprint(got) != "[create:pending webhook:assigned webhook:delivered]" {
		t.Errorf("history = %v", got)
	}
}

func TestMarkEvent(t *testing.T) {
	s := NewStore(Limits{MaxDeliveries: 10, MaxQuotes: 10, MaxSeenEvents: 2, SeenEventTTL: time.Hour})
	if s.MarkEvent("evt_1", t0) {
		t.Error("first sighting reported as duplicate")
	}
	if !s.MarkEvent("evt_1", t0.Add(time.Minute)) {
		t.Error("second sighting not reported as duplicate")
	}
	if s.MarkEvent("evt_1", t0.Add(2*time.Hour)) {
		t.Error("sighting after the TTL should count as new")
	}
	// Beyond the cap the set stays bounded.
	for i := range 10 {
		s.MarkEvent(fmt.Sprintf("evt_x%d", i), t0.Add(3*time.Hour))
	}
	s.mu.RLock()
	n := len(s.seen)
	s.mu.RUnlock()
	if n > 2 {
		t.Errorf("seen set has %d entries, cap is 2", n)
	}
}

func TestReadsAreCopies(t *testing.T) {
	s := NewStore(DefaultLimits())
	s.AddDelivery(newDelivery(1, t0))
	d, _ := s.Delivery("d_1")
	d.History[0].Status = StatusDelivered
	d.Status = StatusDelivered

	again, _ := s.Delivery("d_1")
	if again.History[0].Status != StatusPending || again.Status != StatusPending {
		t.Errorf("mutating a returned delivery changed the store: %+v", again)
	}
}

func TestStatusTerminal(t *testing.T) {
	for s, want := range map[Status]bool{StatusPending: false, StatusAssigned: false, StatusPickedUp: false, StatusDelivered: true} {
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", s, !want)
		}
	}
}

func TestDeliveryByQuote(t *testing.T) {
	s := NewStore(DefaultLimits())
	s.AddDelivery(newDelivery(1, t0))
	if d, ok := s.DeliveryByQuote("q_1"); !ok || d.ID != "d_1" {
		t.Errorf("DeliveryByQuote(q_1) = %s, %v", d.ID, ok)
	}
	if _, ok := s.DeliveryByQuote("q_2"); ok {
		t.Error("DeliveryByQuote(q_2) found a delivery")
	}
}
