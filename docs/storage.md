[← Docs](README.md)

# Storage, behaviour and cost

## What Sonda stores, and what that means

Sonda writes the bytes that crossed the wire into a SQLite file. **That
includes whatever your traffic carries**: `Authorization` headers, session
cookies, API keys, personal data. Application payloads are not redacted on the
way in, and that is a deliberate choice rather than an oversight — redacting a
replayable capture would mean it is no longer what was sent.

Two connection-handshake exceptions make the safer trade-off: a
**Postgres** password and **AMQP SASL challenges and responses** are blanked in
the stored copy as the bytes go past. Neither capture can be replayed without
the connection state that is already gone, and the alternative is a live
credential in a plaintext file. See [PostgreSQL](protocols.md#postgresql) and
[AMQP 0-9-1](protocols.md#amqp-0-9-1).

The other place credentials are held back is [the MCP server](agents.md), because
there the answers leave the machine.

What follows from that:

- **The database is a plain file with no encryption**, wherever `database:`
  points. Treat it like a log file containing credentials, because it is one.
- **Sonda has no authentication.** Anyone who can reach its port can read
  every capture. Bind it to `127.0.0.1` — the shipped configuration does — and
  do not publish the port.
- **It is a local development tool.** Pointing it at production traffic is
  outside what it was built for and outside what its threat model covers.
- Retention bounds how long captures live, but it is a housekeeping limit, not a
  security control.

## Behaviour worth knowing

- **Capture never delays traffic.** Writes happen on a separate goroutine behind
  a bounded buffer. Under a burst, captures are dropped rather than slowing the
  system you are trying to observe — and `GET /api/stats` reports the drop count,
  so the loss is visible instead of silent.
- **Large bodies are truncated in storage only.** A 500 KB request with a
  256 KB cap is forwarded in full and stored as 256 KB, flagged `truncated`,
  with `size` reporting the real 500 KB.
- **Badly encoded text is still searchable.** A latin-1 accent from a service
  that never got its charset right does not make the call unfindable; the index
  is sanitized while the stored bytes stay exact. Genuinely binary payloads are
  not indexed.
- **An unreachable upstream is captured too.** Sonda answers 502 and records
  the transport error, so the failure is in the timeline rather than missing
  from it. A 502 that came from the upstream itself has an empty `error` field.
- **Retention runs on a timer**, applying age first and then the row cap.
- **Ctrl+C drains the buffer** before exiting.

## What it costs

Sonda is built on the claim that a debugger which alters the traffic invalidates
every conclusion drawn from it, and timing is part of what a proxy alters. If
you are chasing a timeout at a service boundary, you are entitled to know what
the instrument in the middle added. So the proxy is benchmarked against itself,
in `internal/proxy/bench_test.go`.

| HTTP, 256-byte body | µs per call |
|---|---|
| Straight to the upstream, no proxy | ~157 |
| Through a stock `httputil.ReverseProxy`, no Sonda | ~430 |
| Through Sonda, capturing | ~540 |
| Through Sonda, recorder replaced by a no-op | ~452 |

| gRPC unary | µs per call |
|---|---|
| Straight to the service, no proxy | ~252 |
| Through a stock reverse proxy, no Sonda | ~797 |
| Through Sonda, capturing | ~960 |

What those rows say:

- **Most of the added latency is the price of being a proxy at all, not of
  capturing.** For small HTTP, the second TCP hop plus `ReverseProxy` itself
  costs ~273 µs, and Sonda's own share on top of that is ~110 µs. gRPC is the
  same shape: ~545 µs for the bare proxy, ~163 µs added by Sonda. Anyone who
  puts any proxy in the path pays the first part; only the second is Sonda's.
- **Most of Sonda's own share is the recorder** — about 88 µs of the 110. That
  is not the request waiting on a database write: `Record` is a non-blocking
  send onto a buffered channel, and it drops rather than blocking. It is the
  capture being built and its bodies copied on the request's own goroutine, plus
  the draining goroutine writing SQLite on the same CPUs. It is a real cost of
  the design and worth stating rather than hiding.
- **Streaming is measured as per-message lag**, not as throughput, because total
  stream time is dominated by whatever the server waits between messages. Direct
  is ~1.63 ms and through Sonda ~2.20 ms, so a message arrives roughly 0.6 ms
  later. The absolute figures are not transit time — they include the test
  server's own sleep overshooting, which the arithmetic charges to transit. Both
  sides run against the same server, so only the difference means anything.
- **Capturing a large body allocates a copy of it.** A 1 MiB request and a 1 MiB
  response, with `max_body_bytes` set above the body, allocate ~7.4 MB per call;
  the same call with the body past the cap allocates ~2.3 MB. That cost shows up
  as memory pressure rather than as latency, and `max_body_bytes` is the knob
  that controls it.

Roughly a tenth of a millisecond on top of what any proxy already costs is a
small number for a tool that runs on the same machine as the services it
watches. Whether it is small enough is a judgement about what you are debugging.

**These figures are a measurement, not a specification.** They come from one
laptop — an Intel Core i9-14900HX on Windows — over loopback, against `httptest`
servers, under whatever else that machine happened to be doing, at
`-benchtime=2000x -count=5`, taking the middle of the five runs. Nothing here is
promised: another machine, a real network, or a busier one will produce other
numbers. If the cost matters to a decision you are making, run them yourself:

```bash
go test ./internal/proxy/ -bench=. -benchmem -run=XXX
```

`CONTRIBUTING.md` says how to read the output, which is less obvious than it
looks.

