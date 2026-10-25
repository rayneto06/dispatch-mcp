//go:build smoke

// Package smoke drives a deployed dispatch-mcp over Streamable HTTP, the way
// a remote agent would:
//
//	MCP_URL=https://.../mcp/<token> go test -tags smoke -v ./internal/smoke
package smoke

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHostedFlow(t *testing.T) {
	url := os.Getenv("MCP_URL")
	if url == "" {
		t.Skip("MCP_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 5 {
		t.Errorf("%d tools, want 5", len(tools.Tools))
	}

	call := func(name string, args any) map[string]any {
		t.Helper()
		start := time.Now()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		if res.IsError {
			b, _ = json.Marshal(res.Content)
		}
		t.Logf("%s (%v, error=%v):\n%s", name, time.Since(start).Round(time.Millisecond), res.IsError, b)
		if res.IsError {
			t.Fatalf("%s returned a tool error", name)
		}
		out, _ := res.StructuredContent.(map[string]any)
		return out
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

	booked := call("create_delivery", map[string]any{"quote_id": cheapest})
	id := booked["delivery"].(map[string]any)["delivery_id"]
	call("get_delivery", map[string]any{"delivery_id": id})
	call("list_deliveries", map[string]any{})
	call("partner_health", map[string]any{})
}
