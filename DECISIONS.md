# Design decisions

Each entry: what was chosen, what it costs, and what a production version
would do differently.

## Shape

**Two binaries, one module.** `dispatch-mcp` is the product; `fakefleet`
plays three partners so there is something realistic, and badly behaved, to
integrate with. They share one package, `webhooksig`, because sender and
receiver must agree byte for byte. The partner API's wire types are written
separately on each side, as they would be against a real partner whose code
you don't get.

**Interfaces where the service meets the outside.** `dispatch.Service`
depends on `Partner` and `Router` interfaces declared next to it.
`*partner.Client` and `*routing.OSRM` satisfy them without naming them;
tests substitute in-memory fakes. There are no other interfaces.

**Few dependencies.** The MCP SDK and the standard library, plus
`golang.org/x/time/rate`, which the SDK already depends on. A token bucket
written by hand would be more code to get right for no gain.

## MCP

**Stateless Streamable HTTP when hosted, stdio locally.** Stateless mode
keeps no `Mcp-Session-Id`, so a Cloud Run restart or a second instance never
strands a client on a dead session, and it matches the sessionless direction
of the 2026-07-28 protocol revision. The cost is no server-to-client
requests, which these tools don't need.

**A secret URL instead of OAuth.** The hosted endpoint is
`/mcp/{token}`, compared in constant time, 404 on mismatch. Every MCP client
accepts a URL; not all let you set an auth header. The token appears in
request logs of the project that hosts it. Production: OAuth as the MCP
authorization spec describes, with per-client identity and rate limits.

**Errors are written for the model.** A tool returns errors as tool results
(`IsError`), not protocol errors, so the model can read them and recover.
Each known failure maps to a sentence that says what to do next ("quote
expired; call quote_delivery again"). Unknown failures are logged and
reported only by request ID.

**Health as a tool.** `partner_health` exposes the integration counters to
the agent, so "did any partner misbehave?" has an answer from the server's
own numbers. Counters are in-process and reset on restart. Production:
metrics and traces in a real backend, and this tool reading from it.

## Calling partners

**The quote ID is the idempotency key.** It is stable for the logical
booking: the same on every retry and on every repeated `create_delivery`.
The partner turns a retry after a lost response into a replay, and the
store refuses a second delivery for the same quote, so concurrent calls
converge on one booking. Asking the agent for a key would add a way to get
it wrong.

**Retry only when it is safe and useful.** Safe: a GET, a POST with no side
effects (quotes), or a POST carrying an idempotency key. Useful: 429, 5xx,
timeouts and connection errors; never other 4xx. At most three attempts.

**Backoff with full jitter.** A random wait between zero and a cap that
doubles per attempt (200 ms, 400 ms, up to 2 s), so callers that failed
together don't retry together.

**`Retry-After` is honored, capped, and never outlives the deadline.** The
wait is capped at 5 s. If it would end after the caller's deadline, the
client fails at once with the retry-after value in the error instead of
sleeping into a timeout.

**One deadline for the quote round, one goroutine per partner.** Results go
to a channel buffered to one slot per partner. The collector stops at the
deadline; a partner that answers later can still send into its slot, so its
goroutine always exits. Late partners are reported as "timed out", which is
the useful answer for the agent. OSRM gets its own 1 s budget before the
round starts.

**OSRM degrades instead of failing.** Calls share a 1 request/s limiter (the
demo server's usage policy) and fall back to a straight-line estimate (×1.3,
25 km/h), labelled as such in the result.

## Receiving webhooks

**Signature over timestamp and raw body.** `t=<unix>,v1=<hex HMAC-SHA256>`
over `"<t>.<body>"`, checked with `hmac.Equal` before the body is parsed.
Timestamps outside ±5 minutes are rejected, which closes the window for
replaying a captured request. Bodies are capped at 64 KiB. One secret is
shared by the three simulated partners; production would have one per
partner, with rotation.

**Deduplicate by event ID, order by sequence.** Partners deliver at least
once. A bounded, expiring set of event IDs drops exact repeats. Each event
carries the partner's sequence number for the delivery, and only a newer one
is applied, so late or reordered events can't move a delivery backwards.
Either check alone would miss a case: a resent event has a known ID, a
reordered one does not.

**Early webhooks are held.** A partner can report "assigned" before we have
its booking response, for example when it books, answers 503, and our retry
comes back later. The receiver still answers 200, and the store keeps the
update (bounded, 10 minutes) and applies it when the delivery is recorded.
An end-to-end test found this race.

**Polling is the backstop.** If a delivery has not heard from its partner in
30 s, `get_delivery` asks the partner directly, within 3 s. A failed refresh
is a note on the answer, not an error. Webhooks keep things fast; polling
keeps them correct when a webhook is lost.

**The fake partner duplicates on purpose.** 10–30% of events are sent twice
so deduplication shows up in `partner_health` instead of only in tests.

## Running it

**State in memory, one instance, scale to zero.** Both services run with
`max-instances=1`, which correctness needs (state lives in the process) and
which also caps cost. With `min-instances=0`, state resets after about 15
idle minutes; a delivery completes in two to three, so only history is lost. The
next step would be a snapshot to a Cloud Storage volume on SIGTERM, reloaded
at start. Production: a database, and webhook ingestion as its own service
writing to it.

**Waking the partner.** When `dispatch-mcp` starts, and on any MCP request
after 10 idle minutes, it pings the fleet in the background. An agent
connects seconds before its first quote, so the partners are normally awake
by then; if not, the quote error says to retry in a few seconds.

**CPU outside requests for the fake fleet.** It advances statuses and sends
webhooks from background goroutines, which Cloud Run's request-based billing
would freeze. `fakefleet` uses instance-based billing; `dispatch-mcp`, which
only works during requests, does not.

**Graceful shutdown.** `signal.NotifyContext` cancels a root context on
SIGTERM. HTTP servers drain for up to 8 s; the fleet stops its timers and
gives queued webhooks what is left of Cloud Run's 10 s.

**Health checks on `/health`.** On the first deploy, `/healthz` got a
Google-branded 404 that never reached the container (others report the
same; it is not documented), so both services use `/health`.

**Explicit HTTP server timeouts.** The zero values mean no timeout, which
lets a slow client hold a connection forever.

**Secrets in Secret Manager.** Generated once by the deploy script, mounted
as environment variables, readable only by a dedicated service account.

**Logs Cloud Logging can parse.** `log/slog` JSON with `severity` and
`message` keys. A request ID created per MCP request travels in the context
to every partner call and to the partner as `X-Request-ID`.

**Abuse limits for a public demo.** A global token bucket on `/mcp`
(5 req/s, bursts of 20), caps on stored quotes, deliveries and event IDs,
the OSRM limiter, body size caps, `max-instances=1`, and a billing spend cap
on the project. Per-client limits would need per-client identity; see OAuth
above.

## Next steps

**Tracing.** The two seams already exist. The MCP receiving middleware
(`internal/mcpserver/server.go`) wraps every request and measures it: that
becomes one span per tool call. `partner.Client.attempt`
(`internal/partner/client.go`) already logs partner, status, attempt and
duration: that becomes one child span per HTTP attempt, so retries show up
as siblings. The request ID that travels today in the context and in
`X-Request-ID` would become a W3C `traceparent`, exported to Cloud Trace.
Not built here: it adds several OpenTelemetry modules, and for someone using
the server a span exporter shows little that the request ID doesn't.

**Tool-selection evals.** A JSON file of scenarios (a user request, the tool
an agent should call, the argument keys it must pass, and cases where no
tool should be called), run by a test behind an `eval` build tag: start the
server in-process over in-memory transports, list its tools, send each
scenario and the tool schemas to a model, and check the first tool call's
name and arguments. The scenario file is the regression set: a change to a
tool description that makes the model pick the wrong tool fails the run.
Task-completion evals would follow, scripted against the fake fleet, whose
faults make "did the agent recover?" measurable.

**Also not here:** persistence, OAuth, per-partner secrets, Terraform and
Kubernetes. Each is a real next step; none was needed to show the
integration patterns working.
