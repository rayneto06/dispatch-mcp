package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch"
	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/obs"
	"github.com/rayneto06/dispatch-mcp/internal/partner"
)

type tools struct {
	svc    *dispatch.Service
	health *health.Registry
	logger *slog.Logger
}

// Tool inputs and outputs. The SDK infers each tool's JSON schema from these
// types, with descriptions from the jsonschema tags.

type quoteInput struct {
	Pickup  geo.Point `json:"pickup" jsonschema:"where the courier collects the package"`
	Dropoff geo.Point `json:"dropoff" jsonschema:"where the package is delivered"`
}

type createInput struct {
	QuoteID string `json:"quote_id" jsonschema:"a quote_id returned by quote_delivery"`
}

type createOutput struct {
	Delivery      dispatch.Delivery `json:"delivery"`
	AlreadyBooked bool              `json:"already_booked" jsonschema:"true when this quote was booked before; the existing delivery is returned"`
}

type getInput struct {
	DeliveryID string `json:"delivery_id" jsonschema:"a delivery_id returned by create_delivery or list_deliveries"`
}

type getOutput struct {
	Delivery dispatch.Delivery `json:"delivery"`
	Note     string            `json:"note,omitempty" jsonschema:"set when the status could not be refreshed from the partner"`
}

type listInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page; omit for the first page"`
	Limit  int    `json:"limit,omitempty" jsonschema:"page size, 1 to 50 (default 10)"`
}

type listOutput struct {
	Deliveries []dispatch.Delivery `json:"deliveries"`
	NextCursor string              `json:"next_cursor" jsonschema:"pass to the next call; empty on the last page"`
}

type healthInput struct{}

func ptr[T any](v T) *T { return &v }

func (t *tools) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "quote_delivery",
		Description: "Get delivery quotes from every fleet partner at once for a pickup and a dropoff (WGS84 lat/lon, up to 50 km apart). " +
			"Returns quotes sorted by price, each with a quote_id valid for a few minutes, and the partners that did not answer in time, with the reason.",
		Annotations: &mcp.ToolAnnotations{Title: "Quote a delivery", ReadOnlyHint: true},
	}, t.quote)

	mcp.AddTool(s, &mcp.Tool{
		Name: "create_delivery",
		Description: "Book a delivery from a quote_id returned by quote_delivery. Idempotent: calling it again with the same quote_id " +
			"returns the same delivery with already_booked true, so it is safe to retry after a timeout or error.",
		Annotations: &mcp.ToolAnnotations{Title: "Book a delivery", IdempotentHint: true, DestructiveHint: ptr(false)},
	}, t.create)

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_delivery",
		Description: "Get a delivery's current status and its status history. Partners push updates by webhook; " +
			"if a partner has been quiet for a while, the server asks it directly before answering.",
		Annotations: &mcp.ToolAnnotations{Title: "Track a delivery", ReadOnlyHint: true},
	}, t.get)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_deliveries",
		Description: "List deliveries, newest first, one page at a time. Pass next_cursor from the previous page to continue.",
		Annotations: &mcp.ToolAnnotations{Title: "List deliveries", ReadOnlyHint: true},
	}, t.list)

	mcp.AddTool(s, &mcp.Tool{
		Name: "partner_health",
		Description: "Integration health per partner since the server started: requests, retries, 429s, 5xx, timeouts, webhooks received, " +
			"duplicate and out-of-order webhooks dropped, bad signatures; plus OSRM routing calls and fallbacks.",
		Annotations: &mcp.ToolAnnotations{Title: "Partner health", ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, t.partnerHealth)
}

func (t *tools) quote(ctx context.Context, _ *mcp.CallToolRequest, in quoteInput) (*mcp.CallToolResult, dispatch.QuoteResult, error) {
	res, err := t.svc.QuoteAll(ctx, in.Pickup, in.Dropoff)
	if err != nil {
		return nil, dispatch.QuoteResult{}, t.toolError(ctx, err)
	}
	return nil, res, nil
}

func (t *tools) create(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, createOutput, error) {
	d, already, err := t.svc.Create(ctx, in.QuoteID)
	var apiErr *partner.APIError
	if errors.As(err, &apiErr) && !apiErr.Rejected() {
		// Only the last attempt is in the error. An earlier one may have
		// booked (the partner committed, then failed to answer), so the
		// safe advice is the idempotent retry, not a second partner.
		return nil, createOutput{}, fmt.Errorf("%v; the booking may still have gone through, "+
			"so call create_delivery again with the same quote_id before booking another quote", err)
	}
	if err != nil {
		return nil, createOutput{}, t.toolError(ctx, err)
	}
	return nil, createOutput{Delivery: d, AlreadyBooked: already}, nil
}

func (t *tools) get(ctx context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	d, note, err := t.svc.Get(ctx, in.DeliveryID)
	if err != nil {
		return nil, getOutput{}, t.toolError(ctx, err)
	}
	return nil, getOutput{Delivery: d, Note: note}, nil
}

func (t *tools) list(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
	ds, next, err := t.svc.List(in.Cursor, in.Limit)
	if err != nil {
		return nil, listOutput{}, t.toolError(ctx, err)
	}
	return nil, listOutput{Deliveries: ds, NextCursor: next}, nil
}

func (t *tools) partnerHealth(context.Context, *mcp.CallToolRequest, healthInput) (*mcp.CallToolResult, health.Snapshot, error) {
	return nil, t.health.Snapshot(), nil
}

// toolError turns an error into a message an agent can act on. A returned
// error becomes a tool result with IsError set, which the model sees; a
// protocol error would hide it. Anything unexpected is logged and reported
// by request ID only.
func (t *tools) toolError(ctx context.Context, err error) error {
	var apiErr *partner.APIError
	switch {
	case errors.Is(err, dispatch.ErrQuoteNotFound):
		return errors.New("quote_id not found; call quote_delivery to get a fresh quote")
	case errors.Is(err, dispatch.ErrQuoteExpired):
		return errors.New("quote expired; call quote_delivery again")
	case errors.Is(err, dispatch.ErrDeliveryNotFound):
		return errors.New("delivery_id not found. This demo keeps state in memory and resets after about 15 minutes without traffic")
	case errors.Is(err, dispatch.ErrOutOfArea):
		return errors.New("pickup and dropoff are more than 50 km apart; this service only handles last-mile deliveries")
	case errors.Is(err, dispatch.ErrSamePoint):
		return errors.New("pickup and dropoff are the same point")
	case errors.Is(err, dispatch.ErrInvalidCursor):
		return errors.New("invalid cursor; omit it to start from the first page")
	case errors.Is(err, dispatch.ErrCapacity):
		return errors.New("demo capacity reached; try again later")
	case errors.Is(err, dispatch.ErrInvalidLocation), errors.Is(err, dispatch.ErrNoQuotes):
		return err
	case errors.Is(err, partner.ErrRateLimited):
		return fmt.Errorf("%v; try again after the retry-after delay or book another partner's quote", err)
	case errors.Is(err, partner.ErrUnavailable):
		return fmt.Errorf("%v; try again shortly or book another partner's quote", err)
	case errors.As(err, &apiErr):
		return fmt.Errorf("%v; repeating the same call is safe", err)
	}
	id := obs.RequestID(ctx)
	t.logger.Error("tool failed", "request_id", id, "error", err)
	return fmt.Errorf("internal error (request %s)", id)
}
