package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch"
	"github.com/rayneto06/dispatch-mcp/internal/dispatch/dispatchtest"
	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/partner"
)

var (
	ferryBuilding  = geo.Point{Lat: 37.7955, Lon: -122.3937}
	missionDolores = geo.Point{Lat: 37.7599, Lon: -122.4268}
)

func newSession(t *testing.T, partners ...*dispatchtest.Partner) (*mcp.ClientSession, *atomic.Int32) {
	t.Helper()
	names := make([]string, len(partners))
	ps := make([]dispatch.Partner, len(partners))
	for i, p := range partners {
		names[i], ps[i] = p.PartnerName, p
	}
	reg := health.NewRegistry(names, time.Now())
	svc := dispatch.NewService(dispatch.DefaultConfig(), dispatch.NewStore(dispatch.DefaultLimits()), dispatchtest.Router{}, ps, reg, time.Now)
	kicks := &atomic.Int32{}
	server := New(Deps{Service: svc, Health: reg, Logger: slog.New(slog.DiscardHandler), OnRequest: func() { kicks.Add(1) }})

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, kicks
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func structured(t *testing.T, res *mcp.CallToolResult, out any) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
	return string(b)
}

func TestToolsListed(t *testing.T) {
	cs, _ := newSession(t, &dispatchtest.Partner{PartnerName: "alpha"})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	for _, name := range []string{"quote_delivery", "create_delivery", "get_delivery", "list_deliveries", "partner_health"} {
		tool, ok := got[name]
		if !ok {
			t.Errorf("tool %s missing", name)
			continue
		}
		if tool.Description == "" || tool.Annotations == nil {
			t.Errorf("tool %s has no description or annotations", name)
			continue
		}
		wantReadOnly := name != "create_delivery"
		if tool.Annotations.ReadOnlyHint != wantReadOnly {
			t.Errorf("%s ReadOnlyHint = %v", name, tool.Annotations.ReadOnlyHint)
		}
	}
	if c := got["create_delivery"]; c != nil && (!c.Annotations.IdempotentHint || c.Annotations.DestructiveHint == nil || *c.Annotations.DestructiveHint) {
		t.Errorf("create_delivery annotations = %+v, want idempotent and non-destructive", c.Annotations)
	}
	if len(res.Tools) != 5 {
		t.Errorf("%d tools, want 5", len(res.Tools))
	}
}

func TestToolErrorsAreAgentReadable(t *testing.T) {
	gamma := &dispatchtest.Partner{PartnerName: "gamma", CreateFn: func(context.Context, string, partner.CreateRequest) (partner.Delivery, error) {
		return partner.Delivery{}, &partner.APIError{Partner: "gamma", StatusCode: 503, Attempts: 3}
	}}
	cs, _ := newSession(t, gamma)
	var quoted dispatch.QuoteResult
	structured(t, call(t, cs, "quote_delivery", map[string]any{"pickup": ferryBuilding, "dropoff": missionDolores}), &quoted)

	tests := []struct {
		name, tool string
		args       any
		want       string
	}{
		{"unknown quote", "create_delivery", map[string]any{"quote_id": "q_nope"}, "quote_id not found; call quote_delivery to get a fresh quote"},
		{"unknown delivery", "get_delivery", map[string]any{"delivery_id": "d_nope"}, "resets after about 15 minutes without traffic"},
		{"bad latitude", "quote_delivery", map[string]any{"pickup": geo.Point{Lat: 200}, "dropoff": missionDolores}, "lat must be between"},
		{"same point", "quote_delivery", map[string]any{"pickup": ferryBuilding, "dropoff": ferryBuilding}, "pickup and dropoff are the same point"},
		{"too far", "quote_delivery", map[string]any{"pickup": ferryBuilding, "dropoff": geo.Point{Lat: 37.3382, Lon: -121.8863}}, "more than 50 km apart"},
		{"bad cursor", "list_deliveries", map[string]any{"cursor": "%%%"}, "invalid cursor; omit it to start from the first page"},
		{"partner down", "create_delivery", map[string]any{"quote_id": quoted.Quotes[0].ID}, "partner gamma: unavailable (503) after 3 attempts; try again shortly or book another partner's quote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, cs, tt.tool, tt.args)
			if !res.IsError || !strings.Contains(text(res), tt.want) {
				t.Errorf("IsError=%v text=%q, want error containing %q", res.IsError, text(res), tt.want)
			}
		})
	}
}

func TestQuoteStructuredOutput(t *testing.T) {
	cs, _ := newSession(t, &dispatchtest.Partner{PartnerName: "alpha"})
	raw := structured(t, call(t, cs, "quote_delivery", map[string]any{"pickup": ferryBuilding, "dropoff": missionDolores}), nil)
	for _, want := range []string{`"route":{`, `"source":"osrm"`, `"quote_id":"q_`, `"unavailable":[]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("structured content %s missing %s", raw, want)
		}
	}
}

func TestBookTrackAndHealth(t *testing.T) {
	cs, kicks := newSession(t, &dispatchtest.Partner{PartnerName: "alpha"})
	var quoted dispatch.QuoteResult
	structured(t, call(t, cs, "quote_delivery", map[string]any{"pickup": ferryBuilding, "dropoff": missionDolores}), &quoted)

	var created struct {
		Delivery      dispatch.Delivery `json:"delivery"`
		AlreadyBooked bool              `json:"already_booked"`
	}
	structured(t, call(t, cs, "create_delivery", map[string]any{"quote_id": quoted.Quotes[0].ID}), &created)
	if created.AlreadyBooked || created.Delivery.Status != dispatch.StatusPending {
		t.Errorf("create = %+v", created)
	}
	structured(t, call(t, cs, "create_delivery", map[string]any{"quote_id": quoted.Quotes[0].ID}), &created)
	if !created.AlreadyBooked {
		t.Error("second create_delivery should report already_booked")
	}

	var got struct {
		Delivery dispatch.Delivery `json:"delivery"`
	}
	structured(t, call(t, cs, "get_delivery", map[string]any{"delivery_id": created.Delivery.ID}), &got)
	if got.Delivery.ID != created.Delivery.ID || len(got.Delivery.History) != 1 {
		t.Errorf("get = %+v", got.Delivery)
	}

	var listed struct {
		Deliveries []dispatch.Delivery `json:"deliveries"`
		NextCursor string              `json:"next_cursor"`
	}
	structured(t, call(t, cs, "list_deliveries", map[string]any{}), &listed)
	if len(listed.Deliveries) != 1 || listed.NextCursor != "" {
		t.Errorf("list = %+v", listed)
	}

	var snap health.Snapshot
	structured(t, call(t, cs, "partner_health", map[string]any{}), &snap)
	if len(snap.Partners) != 1 || snap.Partners[0].Partner != "alpha" {
		t.Errorf("health = %+v", snap)
	}
	if kicks.Load() == 0 {
		t.Error("OnRequest was never called")
	}
}
