# dispatch-mcp

[![ci](https://github.com/rayneto06/dispatch-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/rayneto06/dispatch-mcp/actions/workflows/ci.yml)

An MCP server, written in Go, that lets an AI agent quote, book and track
last-mile deliveries across several fleet partners.

The agent never talks to a partner. It calls five tools; the server does
the integration work behind them: one REST client per partner with
idempotent bookings and careful retries, HMAC-signed webhooks coming back,
deduplication, ordering, reconciliation, and counters that let the agent see
how the partners behaved.

The partners are simulated (`cmd/fakefleet`), and they misbehave on
purpose: one is slow, one rate-limits and fails, and all of them sometimes
deliver the same webhook twice. Routing uses the public
[OSRM](https://project-osrm.org/) demo server.

This is a learning project, built to get hands-on with Go. It runs on Cloud
Run, but it is a proof of concept, not a production system;
[DECISIONS.md](DECISIONS.md) says what would change for production.

## Architecture

```
agent (Claude Code, MCP Inspector, ...)
   │ MCP: stdio locally, Streamable HTTP when hosted
   ▼
dispatch-mcp ─────────── GET ──────────► OSRM (route distance and time)
   │    ▲
   │    │ POST /webhooks/{partner}   HMAC-signed status events
   │    │
   ▼    │ REST /{partner}/v1/...     API key, Idempotency-Key
fakefleet: partners alpha, beta, gamma
```

| Partner | Behavior |
|---|---|
| alpha | reliable, mid-priced |
| beta | slow to quote (0.8–2.5 s), often misses the 1.5 s quote round |
| gamma | cheapest; 15% 429 with `Retry-After`, 15% 503, and 10% "books the delivery, then answers 503" |

Deliveries go `pending → assigned → picked_up → delivered` in two to three
minutes, each step reported by webhook.

## Tools

| Tool | What it does |
|---|---|
| `quote_delivery` | Routes the trip once (OSRM, or a straight-line estimate if OSRM is down), then asks every partner for a price at the same time and waits at most 1.5 s. Returns quotes sorted by price and the partners that did not answer, with the reason. |
| `create_delivery` | Books a quote with its partner. Idempotent per `quote_id`: repeating the call, even concurrently, returns the same delivery. |
| `get_delivery` | Current status and history. If the partner has been quiet for 30 s, asks it directly before answering, so a lost webhook only delays an update. |
| `list_deliveries` | Newest first, cursor-paginated. |
| `partner_health` | Per partner: requests, retries, 429s, 5xx, timeouts, webhooks received, duplicates and out-of-order events dropped, bad signatures. Plus OSRM calls and fallbacks. |

## A session against the hosted server

Tool calls made by `internal/smoke` against the Cloud Run deployment
(abridged), from the Ferry Building to Mission Dolores in San Francisco:

```jsonc
// quote_delivery {"pickup":{"lat":37.7955,"lon":-122.3937},"dropoff":{"lat":37.7599,"lon":-122.4268}}   2.2 s
{
  "route": {"distance_km": 6.8, "duration_min": 11, "source": "osrm"},
  "quotes": [
    {"quote_id": "q_9028947b83f2c361", "partner": "gamma", "price_cents": 1311, "pickup_eta_min": 16, "dropoff_eta_min": 27},
    {"quote_id": "q_9a8cea8add653a48", "partner": "alpha", "price_cents": 1714, "pickup_eta_min": 15, "dropoff_eta_min": 26}
  ],
  "unavailable": [{"partner": "beta", "reason": "timed out after 1.5s"}]
}

// create_delivery {"quote_id":"q_9028947b83f2c361"}   0.4 s
{"already_booked": false, "delivery": {"delivery_id": "d_…", "partner": "gamma", "status": "pending", "last_update_source": "create", ...}}

// get_delivery, about 15 s later: gamma's webhook moved it on
{"delivery": {"status": "assigned", "last_update_source": "webhook", "history": [...]}}
```

## Run it locally

Requires Go 1.27.

```sh
go build -o bin/ ./cmd/...

# Terminal 1: the simulated partners on :8090. TIME_SCALE=0.2 runs five times faster.
PARTNER_API_KEY=dev WEBHOOK_SECRET=dev TIME_SCALE=0.2 bin/fakefleet

# Claude Code starts dispatch-mcp over stdio; it listens for webhooks on 127.0.0.1:8081.
claude mcp add dispatch -e PARTNER_API_KEY=dev -e WEBHOOK_SECRET=dev -- "$PWD/bin/dispatch-mcp"
```

Then ask Claude Code something like "quote a delivery from the Ferry
Building to Mission Dolores and book the cheapest one".

With the [MCP Inspector](https://github.com/modelcontextprotocol/inspector):

```sh
npx @modelcontextprotocol/inspector -e PARTNER_API_KEY=dev -e WEBHOOK_SECRET=dev bin/dispatch-mcp
```

## Use the hosted demo

The hosted endpoint is a URL with a secret path segment, shared privately:

```sh
claude mcp add --transport http dispatch "<MCP_URL>"
```

In the MCP Inspector, choose the Streamable HTTP transport and paste the
same URL.

Both services scale to zero. The first call after a quiet period wakes them
(a few seconds), and state is in memory, so deliveries disappear after about
15 minutes without traffic. The server's instructions tell the agent this.

## Configuration

| Variable | Binary | Default | |
|---|---|---|---|
| `PARTNER_API_KEY` | both | | required; the partners' API key |
| `WEBHOOK_SECRET` | both | | required; HMAC key for webhooks |
| `TRANSPORT` | dispatch-mcp | `stdio` | `stdio` or `http` |
| `MCP_TOKEN` | dispatch-mcp | | required in http mode, 32+ chars; the secret path segment of `/mcp/{token}` |
| `PORT` | both | `8080` / `8090` | http listener |
| `WEBHOOK_ADDR` | dispatch-mcp | `127.0.0.1:8081` | webhook listener in stdio mode |
| `FLEET_BASE_URL` | dispatch-mcp | `http://127.0.0.1:8090` | |
| `PARTNERS` | dispatch-mcp | `alpha,beta,gamma` | |
| `OSRM_BASE_URL` | dispatch-mcp | `https://router.project-osrm.org` | |
| `QUOTE_DEADLINE` | dispatch-mcp | `1.5s` | |
| `WEBHOOK_URL` | fakefleet | `http://127.0.0.1:8081` | where to send webhooks |
| `TIME_SCALE` | fakefleet | `1` | multiplies status timings |
| `LOG_LEVEL` | both | `info` | |

Logs are JSON on stderr, in the shape Cloud Logging reads. Every MCP request
gets a request ID that is logged with each partner call it makes and sent to
the partner as `X-Request-ID`, so one tool call can be followed across both
services:

```sh
gcloud logging read 'jsonPayload.request_id="req_3f9a1c0b7d2e"' --project my-project --format json
```

## Tests

```sh
go test -race ./...

# Against a deployment (MCP_URL comes from .env, see Deploy):
set -a; . ./.env; set +a
go test -tags smoke -v ./internal/smoke
```

CI runs `gofmt`, `go vet` and the race-enabled tests on every push. Unit
tests use `httptest` servers for the partner, OSRM and webhook sides.
`cmd/dispatch-mcp` has end-to-end tests that drive the real server through
an in-memory MCP client against an in-process fleet, including one where the
partner books and then fails, and sends every webhook twice.

## Deploy

`deploy/deploy.sh` builds one image with Cloud Build and deploys two Cloud
Run services, `dispatch-mcp` and `fakefleet`, in their own project. It
creates the project if needed, the secrets in Secret Manager and a service
account that can only read them. It is safe to re-run. The MCP URL carries
the token, so the script writes it to `.env` (gitignored, mode 600) as
`MCP_URL` instead of printing it; `.env.example` lists the variables.

```sh
PROJECT_ID=my-project BILLING_ACCOUNT=XXXXXX-XXXXXX-XXXXXX bash deploy/deploy.sh
```

Both services run with `min-instances=0` and `max-instances=1`. Set a
Cloud Billing spend cap on Cloud Run for the project.

## Layout

```
cmd/dispatch-mcp     main: config, wiring, stdio/http, shutdown
cmd/fakefleet        main for the simulated partners
internal/mcpserver   the five tools, instructions, request logging, error messages
internal/dispatch    quotes and deliveries: fan-out, booking, reconciliation, store
internal/partner     REST client for one partner: retries, Retry-After, typed errors
internal/webhook     webhook receiver: signature, dedup, ordering
internal/webhooksig  signing scheme shared by both sides
internal/routing     OSRM client and fallback estimate
internal/fleet       the simulated partners
internal/health      counters behind partner_health
internal/httpx       server timeouts, graceful shutdown, token and rate-limit middleware
internal/obs         JSON logger and request IDs
internal/geo         coordinates and distance
internal/smoke       smoke test for a deployment (build tag "smoke")
```
