//go:build smoke

// Package smoke drives a deployed dispatch-mcp over Streamable HTTP, the way
// a remote agent would:
//
//	MCP_URL=https://.../mcp/<token> go test -tags smoke -v ./internal/smoke
package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHostedFlow(t *testing.T) {
	url := os.Getenv("MCP_URL")
	if url == "" {
		t.Skip("MCP_URL not set")
	}
	// The URL carries the token, and transport errors quote it: never print it.
	scrub := func(err error) string { return strings.ReplaceAll(fmt.Sprint(err), url, "$MCP_URL") }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect: %s", scrub(err))
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(scrub(err))
	}
	if len(tools.Tools) != 5 {
		t.Errorf("%d tools, want 5", len(tools.Tools))
	}

	// invoke calls a tool and logs the result; call also fails the test on a
	// tool error.
	invoke := func(name string, args any) *mcp.CallToolResult {
		t.Helper()
		start := time.Now()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %s", name, scrub(err))
		}
		b, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		if res.IsError {
			b, _ = json.Marshal(res.Content)
		}
		t.Logf("%s (%v, error=%v):\n%s", name, time.Since(start).Round(time.Millisecond), res.IsError, b)
		return res
	}
	call := func(name string, args any) map[string]any {
		t.Helper()
		res := invoke(name, args)
		if res.IsError {
			t.Fatalf("%s returned a tool error", name)
		}
		out, _ := res.StructuredContent.(map[string]any)
		return out
	}
	errorText := func(res *mcp.CallToolResult) string {
		var s strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				s.WriteString(tc.Text)
			}
		}
		return s.String()
	}

	quote := call("quote_delivery", map[string]any{
		"pickup":  map[string]float64{"lat": 37.7955, "lon": -122.3937}, // Ferry Building, San Francisco
		"dropoff": map[string]float64{"lat": 37.7599, "lon": -122.4268}, // Mission Dolores
	})
	quotes, _ := quote["quotes"].([]any)
	if len(quotes) == 0 {
		t.Fatal("no quotes")
	}
	cheapest := quotes[0].(map[string]any)["quote_id"]

	// The simulated partners fail on purpose. When a booking error says the
	// booking may have gone through, do what it tells an agent to do: repeat
	// the call with the same quote_id, which is idempotent.
	var booked map[string]any
	for attempt := 1; ; attempt++ {
		res := invoke("create_delivery", map[string]any{"quote_id": cheapest})
		if !res.IsError {
			booked, _ = res.StructuredContent.(map[string]any)
			break
		}
		if attempt == 3 || !strings.Contains(errorText(res), "call create_delivery again with the same quote_id") {
			t.Fatalf("create_delivery returned a tool error on attempt %d", attempt)
		}
	}
	id := booked["delivery"].(map[string]any)["delivery_id"]

	// The partner reports "assigned" about 10 s after booking, by webhook
	// from one Cloud Run service to the other.
	deadline := time.Now().Add(40 * time.Second)
	for {
		got := call("get_delivery", map[string]any{"delivery_id": id})
		d := got["delivery"].(map[string]any)
		if d["status"] != "pending" {
			if d["last_update_source"] != "webhook" {
				t.Errorf("status %v arrived by %v, want webhook", d["status"], d["last_update_source"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still pending after 40s: webhooks are not arriving")
		}
		time.Sleep(5 * time.Second)
	}
	call("list_deliveries", map[string]any{})
	call("partner_health", map[string]any{})
}
