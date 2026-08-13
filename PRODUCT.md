# Product

<!-- impeccable:product-schema 1 -->

## Platform

Local developer tool: embedded web interface, terminal client, HTTP API and MCP
server, backed by the same Go capture service and SQLite store.

## Users

One primary user: a backend developer debugging a local microservice stack on his
own machine. He is mid-task and interrupted — something between two services is
not behaving, and the logs do not contain the payload. He is not exploring; he
has a specific broken interaction in mind and needs to see what actually
crossed the wire.

Two usage scenes, both confirmed:

- **Ambient.** Sonda is open on a second monitor while he works, glanced at
  rather than read.
- **Deliberate.** He opens it *because* something broke, and the first thing he
  wants is what failed — not the full stream.

A secondary audience exists but never uses the tool: a technical reviewer
looking at screenshots or a short recording, judging whether the person who
built this understands distributed systems.

## Product Purpose

Replace log-reading as the way to debug traffic between local services. Sonda
sits in front of a service or broker, forwards traffic without changing the
bytes delivered, and makes the exchange inspectable: HTTP and GraphQL, gRPC,
WebSocket and server-sent events, PostgreSQL statements, and AMQP 0-9-1 units.

Success is that the developer stops opening several container log streams to
reconstruct one interaction, and instead sees the interaction itself.

## Positioning

For HTTP, `mitmproxy` already does this well. Sonda is positioned as a local
multi-protocol service-traffic instrument: it observes traffic it did not
originate, preserves protocol-specific failure signals, and presents the same
captures through the API, web interface, terminal and MCP. For gRPC it deframes
messages, preserves trailers and decodes protobuf through reflection, a
descriptor set or, failing both, the wire format itself. PostgreSQL and AMQP use
raw framed listeners rather than pretending to be HTTP.

## Operating Context

- Runs locally: `docker compose up`, or a single binary. No hosted instance, no
  account, no network dependency.
- The observed stack is mixed: some services in containers, some running
  natively under pnpm on Windows. Interposition is explicit — one listening port
  per observed service — so a service that is not configured is not captured.
- The developer's shell is PowerShell 5.1, which mangles quotes passed to
  external executables. Documented examples must work there.
- Realistic scale is **fifteen or more services at once**, not two or three. The
  service a call belongs to is a navigation axis, not a label.
- Traffic volume is high enough that retention is enforced by age and by row
  count, and captures are dropped rather than allowed to slow the traffic being
  observed.

## Capabilities and Constraints

Confirmed and working:

- Explicit per-port proxying of HTTP/1.1, gRPC over HTTP/2, PostgreSQL and AMQP
  0-9-1. WebSocket and server-sent events are captured through HTTP.
- HTTPS and AMQPS listener termination through a generated local authority;
  HTTPS, TLS gRPC and AMQPS upstreams are verified by default.
- Byte-exact forwarding. Decoding is a view computed at display time. Stored
  Postgres credentials and AMQP SASL challenge/response bytes are deliberately
  blanked without changing what the upstream receives.
- Full-text search across paths and text payloads, plus filters by target,
  method, HTTP status, protocol, gRPC status, and failure.
- Every message of a gRPC stream, WebSocket conversation or server-sent event
  stream is captured. AMQP content methods group method, header and body frames
  into direction-specific units.
- Schema resolution by reflection, descriptor set, or a labelled structural view
  of the wire format.
- Retention by age and count; large bodies truncated in storage only, with the
  real size recorded.
- Captures dropped under load are counted and reported, never silently lost.

Constraints future work must respect:

- Compressed gRPC messages are reported as compressed, not decoded.
- PostgreSQL TLS negotiation is forwarded, but traffic after the negotiation is
  opaque; `tls: true` is not supported for a PostgreSQL listener.
- AMQP support is 0-9-1, not 1.0. Captures are not reconstructed transactions,
  cannot be replayed, stubbed or fault-injected, and frames above the 16 MiB
  capture parser limit retain metadata rather than raw frame bytes.
- Kafka and arbitrary TCP protocols have no listener or decoder.
- Single binary, no frontend toolchain. The UI ships embedded in the Go binary.
- Zero authentication. It binds to localhost and observes one developer's own
  machine.

Terminology, fixed by the code and the API: **target** (a configured service),
**call** (the historical API row for one captured exchange or protocol unit),
and **message** (one framed message within a capture). Replay and structural diff
are implemented for compatible request/response captures; replay targets a
configured channel and is deliberately refused for connection-bound captures
such as PostgreSQL, WebSocket and AMQP.

## Brand Commitments

Name and identity: **Sonda**. The interface is a logic analyzer for service
traffic, with the committed color, typography and component system recorded in
`DESIGN.md`. The API, code and English documentation are in English; a maintained
Spanish README provides the public translation. Payloads routinely contain
non-English text, sometimes badly encoded.

## Evidence on Hand

- A working implementation with package, integration and end-to-end coverage.
- Two toy upstreams that produce real capturable traffic on first run:
  `examples/echo` (HTTP: success, slow, failure, echo) and
  `examples/grpcdemo` (gRPC: unary, server streaming, client streaming, and a
  trailers-only failure).
- Real measured behaviour, not projections: byte-exact forwarding verified with
  `cmp` on 500 KB payloads; retention pruning 22 calls to exactly 5; a
  `PermissionDenied` with a percent-decoded Spanish message.
- 15 benchmarks measuring what the instrument costs, always as the difference
  against two baselines — direct, and a stock `httputil.ReverseProxy` with no
  Sonda in it. On one Windows laptop, a small HTTP call costs ~157 µs direct,
  ~430 µs through a bare proxy and ~540 µs through Sonda: most of what a user
  pays is the price of proxying at all, not of capturing. A measurement with
  its conditions, never a specification.
- AMQP forwarding and capture are exercised end to end against a deterministic
  wire-level test broker. This proves the proxy boundary and sanitization but is
  not presented as a RabbitMQ compatibility suite.

No users, no testimonials, no deployment. None may be invented.

### A decision worth the record: full duplex

`Proxy.ServeHTTP` calls `EnableFullDuplex`, and the reason is not a preference.

Without it, a stock `httputil.ReverseProxy` on HTTP/1 truncates responses. The
server drains and closes the request body the moment the response headers go
out, while the transport is still finishing its trailing read of that same
body; the transport sees a read on a closed body and tears down the upstream
connection mid-response. It needs the upstream to answer inside that instant,
so it hit roughly one large POST in two hundred, and only under load.

It was found as an intermittently failing test that read like a flaky one, and
it was silently corrupting traffic in the one tool whose whole premise is not
altering it. The corroboration came later and independently: the benchmark
baseline — a bare `ReverseProxy` with no Sonda code in the path at all —
failed with `unexpected EOF` on its first full run, which is why that baseline
now enables full duplex too.

The general lesson, and the reason this is written down rather than left in a
commit message: an intermittent test failure is a claim about the product until
it has been read as one.

## Product Principles

1. **Fidelity before features.** A debugger that alters the traffic invalidates
   every conclusion drawn from it. Nothing in the interface may imply Sonda
   changed what crossed the wire.
2. **Never say "I can't show you that."** Missing schema, bad encoding, binary
   payloads and truncated captures all degrade to a lesser view, never to a
   blank one.
3. **Label the guesses.** Where the tool infers — a field type read off the wire
   format, a heuristic — it says so. Confident nonsense is worse than an honest
   gap.
4. **The failure is the reason he's here.** Anything that failed must be
   impossible to miss, and reachable without constructing a query.
5. **Observation must not perturb.** Capture never delays the traffic. Loss
   under pressure is acceptable and is reported; added latency is not.

## Accessibility & Inclusion

No formal standard was established. Two product-specific needs are real: the
interface is read while the user is interrupted and impatient, so scanability
outranks density of expression; and payload text is frequently non-English and
occasionally invalid UTF-8, so rendering must not assume ASCII or break on
malformed input.
