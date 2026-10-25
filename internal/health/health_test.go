package health

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSnapshot(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	r := NewRegistry([]string{"alpha", "beta"}, t0)
	r.Partner("alpha").Retries.Add(2)
	r.Partner("alpha").RecordError("boom", t0.Add(time.Minute))
	r.Routing.FallbackEstimates.Add(1)

	s := r.Snapshot()
	if !s.Since.Equal(t0) || len(s.Partners) != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
	alpha, beta := s.Partners[0], s.Partners[1]
	if alpha.Partner != "alpha" || alpha.Retries != 2 || alpha.LastError != "boom" ||
		alpha.LastErrorAt == nil || !alpha.LastErrorAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("alpha = %+v", alpha)
	}
	if beta.Partner != "beta" || beta.LastError != "" || beta.LastErrorAt != nil {
		t.Errorf("beta = %+v", beta)
	}
	if s.Routing.FallbackEstimates != 1 {
		t.Errorf("routing = %+v", s.Routing)
	}

	b, _ := json.Marshal(s)
	for _, key := range []string{`"since"`, `"quote_requests"`, `"duplicates_dropped"`, `"fallback_estimates"`, `"last_error":"boom"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON %s missing %s", b, key)
		}
	}
	if strings.Count(string(b), `"last_error"`) != 1 {
		t.Errorf("last_error should be omitted for beta: %s", b)
	}
}

func TestUnknownPartnerIsNil(t *testing.T) {
	r := NewRegistry([]string{"alpha"}, time.Now())
	if r.Partner("delta") != nil {
		t.Error("Partner(delta) should be nil")
	}
}

func TestConcurrentIncrements(t *testing.T) {
	r := NewRegistry([]string{"alpha"}, time.Now())
	c := r.Partner("alpha")
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Attempts.Add(1)
				c.RecordError("x", time.Now())
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	if got := r.Snapshot().Partners[0].Attempts; got != 10000 {
		t.Errorf("attempts = %d, want 10000", got)
	}
}
