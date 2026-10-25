package dispatch

import (
	"cmp"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits bound the memory a public demo can be made to use.
type Limits struct {
	MaxDeliveries int
	MaxQuotes     int
	MaxSeenEvents int
	SeenEventTTL  time.Duration
	MaxParked     int           // deliveries with updates waiting for their booking
	ParkedTTL     time.Duration // how long such updates are kept
}

func DefaultLimits() Limits {
	return Limits{MaxDeliveries: 500, MaxQuotes: 2000, MaxSeenEvents: 10000, SeenEventTTL: 24 * time.Hour,
		MaxParked: 1000, ParkedTTL: 10 * time.Minute}
}

// Store keeps all state in memory behind one RWMutex. Every read returns a
// copy, so callers can never touch the store's data without the lock.
type Store struct {
	limits Limits

	mu         sync.RWMutex
	quotes     map[string]Quote
	deliveries map[string]*Delivery
	byQuote    map[string]string     // quote ID → delivery ID
	byPartner  map[partnerRef]string // partner's delivery → our delivery ID
	order      []sortKey             // ascending (CreatedAt, ID), for cursor pagination
	seen       map[string]time.Time  // webhook event ID → first seen
	parked     map[partnerRef][]parkedUpdate
}

// parkedUpdate is a status that arrived before we knew the delivery: the
// partner's webhook beat its own booking response to us.
type parkedUpdate struct {
	u  StatusUpdate
	at time.Time
}

type partnerRef struct{ partner, id string }

type sortKey struct {
	created time.Time
	id      string
}

func (a sortKey) compare(b sortKey) int {
	return cmp.Or(a.created.Compare(b.created), cmp.Compare(a.id, b.id))
}

func NewStore(l Limits) *Store {
	return &Store{
		limits:     l,
		quotes:     map[string]Quote{},
		deliveries: map[string]*Delivery{},
		byQuote:    map[string]string{},
		byPartner:  map[partnerRef]string{},
		seen:       map[string]time.Time{},
		parked:     map[partnerRef][]parkedUpdate{},
	}
}

// PutQuotes stores new quotes, first dropping the ones that have expired.
func (s *Store) PutQuotes(qs []Quote, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, q := range s.quotes {
		if !now.Before(q.ExpiresAt) {
			delete(s.quotes, id)
		}
	}
	if len(s.quotes)+len(qs) > s.limits.MaxQuotes {
		return ErrCapacity
	}
	for _, q := range qs {
		s.quotes[q.ID] = q
	}
	return nil
}

func (s *Store) Quote(id string, now time.Time) (Quote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.quotes[id]
	switch {
	case !ok:
		return Quote{}, ErrQuoteNotFound
	case !now.Before(q.ExpiresAt):
		return Quote{}, ErrQuoteExpired
	}
	return q, nil
}

// AddDelivery stores d unless a delivery for the same quote exists, in which
// case it returns that one and true. One quote can never become two
// deliveries, however many times or however concurrently it is booked.
func (s *Store) AddDelivery(d Delivery) (Delivery, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byQuote[d.QuoteID]; ok {
		return s.deliveries[id].clone(), true, nil
	}
	if len(s.deliveries) >= s.limits.MaxDeliveries {
		return Delivery{}, false, ErrCapacity
	}
	stored := d.clone()
	ref := partnerRef{d.Partner, d.PartnerDeliveryID}
	s.deliveries[d.ID] = &stored
	s.byQuote[d.QuoteID] = d.ID
	s.byPartner[ref] = d.ID
	for _, u := range s.takeParked(ref, d.CreatedAt) {
		applyUpdate(&stored, u, d.CreatedAt)
	}

	k := sortKey{d.CreatedAt, d.ID}
	i, _ := slices.BinarySearchFunc(s.order, k, sortKey.compare)
	s.order = slices.Insert(s.order, i, k)
	return stored.clone(), false, nil
}

// DeliveryByQuote returns the delivery booked from a quote, if any.
func (s *Store) DeliveryByQuote(quoteID string) (Delivery, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byQuote[quoteID]
	if !ok {
		return Delivery{}, false
	}
	return s.deliveries[id].clone(), true
}

func (s *Store) Delivery(id string) (Delivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.deliveries[id]
	if !ok {
		return Delivery{}, ErrDeliveryNotFound
	}
	return d.clone(), nil
}

// List returns deliveries newest first. The cursor names the last item of
// the previous page, not an offset, so deliveries created between two calls
// never shift or repeat items on later pages.
func (s *Store) List(cursor string, limit int) ([]Delivery, string, error) {
	if limit <= 0 {
		limit = 10
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	end := len(s.order) // walk backwards from just before end
	if cursor != "" {
		k, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		end, _ = slices.BinarySearchFunc(s.order, k, sortKey.compare) // first index not before the cursor
	}

	out := make([]Delivery, 0, min(limit, end))
	i := end - 1
	for ; i >= 0 && len(out) < limit; i-- {
		d := s.deliveries[s.order[i].id].clone()
		d.History = nil // list is a summary; get_delivery has the history
		out = append(out, d)
	}
	next := ""
	if i >= 0 && len(out) > 0 {
		last := out[len(out)-1]
		next = encodeCursor(sortKey{last.CreatedAt, last.ID})
	}
	return out, next, nil
}

func encodeCursor(k sortKey) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(k.created.UnixNano(), 10) + "|" + k.id))
}

func decodeCursor(c string) (sortKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return sortKey{}, ErrInvalidCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return sortKey{}, ErrInvalidCursor
	}
	nanos, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return sortKey{}, ErrInvalidCursor
	}
	return sortKey{time.Unix(0, nanos).UTC(), id}, nil
}

// ApplyStatus applies a partner's update if it is newer than what we hold.
// Webhooks can arrive late, twice or out of order; the partner's sequence
// number, not arrival order, decides. An update for a delivery we don't know
// yet is parked: its booking response may still be on the way.
func (s *Store) ApplyStatus(u StatusUpdate, now time.Time) ApplyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := partnerRef{u.Partner, u.PartnerDeliveryID}
	id, ok := s.byPartner[ref]
	if !ok {
		s.park(ref, u, now)
		return Unknown
	}
	return applyUpdate(s.deliveries[id], u, now)
}

// applyUpdate changes a delivery owned by the store; hold s.mu for writing.
func applyUpdate(d *Delivery, u StatusUpdate, now time.Time) ApplyResult {
	d.SyncedAt = now
	if u.Sequence <= d.Sequence {
		return Stale
	}
	d.Status = u.Status
	d.Sequence = u.Sequence
	d.UpdatedAt = u.At
	d.LastUpdateSource = u.Source
	d.History = append(d.History, HistoryEntry{Status: u.Status, At: u.At, Source: u.Source})
	return Applied
}

func (s *Store) park(ref partnerRef, u StatusUpdate, now time.Time) {
	if _, ok := s.parked[ref]; !ok && len(s.parked) >= s.limits.MaxParked {
		for k, ups := range s.parked {
			if now.Sub(ups[0].at) >= s.limits.ParkedTTL {
				delete(s.parked, k)
			}
		}
		if len(s.parked) >= s.limits.MaxParked {
			return // still full: drop it; polling reconciles later
		}
	}
	if len(s.parked[ref]) < 8 { // a delivery has only a handful of statuses
		s.parked[ref] = append(s.parked[ref], parkedUpdate{u, now})
	}
}

// takeParked removes and returns the fresh parked updates for ref, oldest
// sequence first.
func (s *Store) takeParked(ref partnerRef, now time.Time) []StatusUpdate {
	ups := s.parked[ref]
	delete(s.parked, ref)
	var out []StatusUpdate
	for _, p := range ups {
		if now.Sub(p.at) < s.limits.ParkedTTL {
			out = append(out, p.u)
		}
	}
	slices.SortFunc(out, func(a, b StatusUpdate) int { return cmp.Compare(a.Sequence, b.Sequence) })
	return out
}

// MarkEvent records a webhook event ID and reports whether it was already
// seen within the TTL. This is what makes webhook redelivery harmless.
func (s *Store) MarkEvent(id string, now time.Time) (duplicate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at, ok := s.seen[id]; ok && now.Sub(at) < s.limits.SeenEventTTL {
		return true
	}
	if len(s.seen) >= s.limits.MaxSeenEvents {
		for k, at := range s.seen {
			if now.Sub(at) >= s.limits.SeenEventTTL {
				delete(s.seen, k)
			}
		}
		// Still full: forget arbitrary entries. A forgotten duplicate is
		// still caught by ApplyStatus's sequence check.
		for k := range s.seen {
			if len(s.seen) < s.limits.MaxSeenEvents {
				break
			}
			delete(s.seen, k)
		}
	}
	s.seen[id] = now
	return false
}
