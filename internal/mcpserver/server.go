// Package mcpserver exposes the dispatch service to AI agents as MCP tools.
package mcpserver

import (
	"context"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/obs"
)

// Instructions are sent to the client at initialization; agents read them
// to learn the flow before calling anything.
const Instructions = `dispatch-mcp books last-mile deliveries with simulated fleet partners (by default alpha, beta and gamma).

Flow: quote_delivery with pickup and dropoff coordinates (lat/lon; look up addresses yourself) → pick a quote → create_delivery with its quote_id → track with get_delivery or list_deliveries.

Statuses go pending → assigned → picked_up → delivered over two to three minutes, pushed by partner webhooks.

create_delivery is idempotent per quote_id: repeating it never books twice, so retrying after an error is safe.

The partners misbehave on purpose (beta is slow to quote, gamma is cheap but flaky). partner_health shows the retries, rate limits, timeouts and duplicate webhooks the server absorbed.

This is a demo: state lives in memory and resets after about 15 minutes without traffic, and the first call after idle can take a few seconds.`

type Deps struct {
	Service   *dispatch.Service
	Health    *health.Registry
	Logger    *slog.Logger
	OnRequest func() // called at the start of every MCP request; may be nil
}

func New(d Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "dispatch-mcp", Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: Instructions, Logger: d.Logger})
	t := &tools{svc: d.Service, health: d.Health, logger: d.Logger}
	t.register(s)
	s.AddReceivingMiddleware(logging(d.Logger, d.OnRequest))
	return s
}

// logging gives every MCP request a request ID (carried in the context down
// to partner calls) and logs one line per request with its duration.
func logging(logger *slog.Logger, onRequest func()) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if onRequest != nil {
				onRequest()
			}
			id := obs.NewRequestID()
			ctx = obs.WithRequestID(ctx, id)
			start := time.Now()

			res, err := next(ctx, method, req)

			attrs := []any{"method", method, "request_id", id, "duration_ms", time.Since(start).Milliseconds()}
			if call, ok := req.(*mcp.CallToolRequest); ok {
				attrs = append(attrs, "tool", call.Params.Name)
				if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
					attrs = append(attrs, "is_error", r.IsError)
				}
			}
			if err != nil {
				logger.Warn("mcp request", append(attrs, "error", err)...)
			} else {
				logger.Info("mcp request", attrs...)
			}
			return res, err
		}
	}
}
