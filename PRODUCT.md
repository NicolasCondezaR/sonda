# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

One primary user: a backend developer debugging a local microservice stack on his
own machine. He is mid-task and interrupted — something between two services is
not behaving, and the logs do not contain the payload. He is not exploring; he
has a specific broken interaction in mind and needs to see what actually
crossed the wire.

Two usage scenes, both confirmed:

- **Ambient.** Mirador is open on a second monitor while he works, glanced at
  rather than read.
- **Deliberate.** He opens it *because* something broke, and the first thing he
  wants is what failed — not the full stream.

A secondary audience exists but never uses the tool: a technical reviewer
looking at screenshots or a short recording, judging whether the person who
built this understands distributed systems.

## Product Purpose

Replace log-reading as the way to debug traffic between local services. Mirador
sits in front of a service, forwards every call untouched, and makes the request
and response inspectable: HTTP with JSON, and gRPC with protobuf decoded to
field names.

Success is that the developer stops opening several container log streams to
reconstruct one interaction, and instead sees the interaction itself.

## Positioning

For HTTP, `mitmproxy` already does this well. For gRPC nothing does: `grpcurl`
and `grpcui` *make* calls, they do not observe the ones services make to each
other. Mirador's specific claim is capturing gRPC traffic it did not originate —
deframing the messages, preserving the trailers where the real status lives, and
decoding protobuf through reflection, a descriptor set, or failing both, the
wire format itself.

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

- Explicit per-port proxying of HTTP/1.1 and gRPC over cleartext HTTP/2.
- Byte-exact forwarding. Bodies are stored as they crossed the wire; decoding is
  a view computed at display time.
- Full-text search across paths and text payloads, plus filters by target,
  method, HTTP status, protocol, gRPC status, and failure.
- Every message of a streaming call is captured, both directions.
- Schema resolution by reflection, descriptor set, or a labelled structural view
  of the wire format.
- Retention by age and count; large bodies truncated in storage only, with the
  real size recorded.
- Captures dropped under load are counted and reported, never silently lost.

Constraints future work must respect:

- Cleartext only. A TLS upstream is forwarded but cannot be inspected.
- Compressed gRPC messages are reported as compressed, not decoded.
- No WebSocket or SSE capture.
- Single binary, no frontend toolchain. The UI ships embedded in the Go binary.
- Zero authentication. It binds to localhost and observes one developer's own
  machine.

Terminology, fixed by the code and the API: **target** (a configured service),
**call** (one captured request/response exchange), **message** (one gRPC frame
within a call).

Undecided: whether replay and diff (planned) operate against the original
upstream only, or against an arbitrary one.

## Brand Commitments

Name: Mirador — provisional, chosen by the author, not yet final. Spanish for a
vantage point, which is the whole idea. No logo, no colors, no typography have
been committed. The API and all code, comments and documentation are in English;
the author is a Chilean Spanish speaker and payloads routinely contain Spanish
text, sometimes badly encoded.

## Evidence on Hand

- A working implementation with 68 passing tests.
- Two toy upstreams that produce real capturable traffic on first run:
  `examples/echo` (HTTP: success, slow, failure, echo) and
  `examples/grpcdemo` (gRPC: unary, server streaming, client streaming, and a
  trailers-only failure).
- Real measured behaviour, not projections: byte-exact forwarding verified with
  `cmp` on 500 KB payloads; retention pruning 22 calls to exactly 5; a
  `PermissionDenied` with a percent-decoded Spanish message.

No users, no testimonials, no benchmarks beyond the above, no deployment. None
may be invented.

## Product Principles

1. **Fidelity before features.** A debugger that alters the traffic invalidates
   every conclusion drawn from it. Nothing in the interface may imply Mirador
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
