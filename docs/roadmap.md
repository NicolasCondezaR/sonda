[← Docs](README.md)

# Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1 | HTTP capture, storage, search, query API | done |
| 2 | gRPC: h2c, trailers, message framing, protobuf decoding | done |
| 3 | Web UI with a live timeline | done |
| 4 | Replay and structural diff | done |
| 5 | Packaging and documentation | done |
| 6 | TUI, as a second client of the same API | done |
| 7 | Projects: grouped services, configured from the interface, imported from a file | done |
| 8 | MCP server, so a coding agent reads the captures itself | done |
| 9 | Configuration over MCP: connect a whole project by asking | done |
| 10 | Correlation: the calls of one request, arranged as a tree | done |
| 11 | Stub mode: answer from a recording instead of forwarding | done |
| 12 | The tree and the stub, on every surface: web, terminal, MCP | done |
| 13 | WebSocket and server-sent events | done |
| 14 | Fault injection: latency, forced statuses, cut connections, armed and read on every surface | done |
| 15 | Contract drift: a field gone, a field retyped | done |
| 16 | GraphQL: the operation behind every identical POST, and its errors counted as failures | done |
| 17 | PostgreSQL: one capture per statement, hung under the request that ran it, with the credentials blanked before they are stored | done |
| 18 | TLS: terminated for the client from a local authority Sonda never installs, spoken to the upstream, and recorded on every capture as verified or not | done |
| 19 | AMQP 0-9-1 and AMQPS: byte-exact relay, useful capture units, SASL sanitization, search and decoded API/MCP/web/TUI views | done |

Kafka is deliberately absent from that table. Why is below.

### Why Kafka is absent

**Sonda has no Kafka listener today.** `protocol:` takes `http`, `grpc`,
`postgres` and `amqp`; Kafka frames arriving at any of those listeners are not
Kafka captures. Nothing in this section is usable yet: it is why the row is
missing, not a recipe.

Putting a proxy in front of a broker does not work, and the reason is not the
protocol. A Kafka client uses its first connection only to ask where the brokers
are. The answer is whatever the broker publishes as its `advertised.listeners`,
and the client then opens **new connections straight to those addresses** and
sends everything that matters — produces, fetches, every group call — down
those. A proxy in the middle sees the handshake and then nothing, forever, while
the traffic someone attached a debugger to flows around it.

Making the client stay would mean rewriting the broker addresses inside those
responses as they cross. Sonda will not, for the reason at the top of
`internal/proxy/proxy.go`: forwarding is byte exact, and a capture of a cluster
that does not exist is worse than no capture at all. Rewriting addresses is what
a Kafka gateway is for, and it is the opposite of this.

There is a way round that rewrites nothing — point the broker at Sonda instead
of putting Sonda in front of it, so every address the client is handed is one it
was meant to dial:

```yaml
# the broker would listen on 9192, Sonda would own 9092
KAFKA_ADVERTISED_LISTENERS: PLAINTEXT://localhost:9092
```

So if Kafka capture is ever built, that is how it will be wired, and what is
left to build is a `kafka` protocol with a raw listener and a decoder beside
`pgwire` and `amqpwire`. The hard part was never the protocol.

### Limitations

- Sonda is an explicit proxy, not an interceptor. It reads a TLS exchange only
  when it is the one terminating the client's connection, which means the
  caller has to be pointed at Sonda and has to trust its authority. Traffic
  that merely passes through on its way elsewhere is forwarded, not decoded.
- Compressed gRPC messages are not decompressed.
- AMQP support is 0-9-1 only. Captures are direction-specific protocol units,
  not reconstructed transactions or publish/delivery pairs, and they cannot be
  replayed, stubbed or fault-injected.
- AMQP frames above the 16 MiB capture parser limit are still forwarded but only
  size and error metadata are stored. The ordinary `max_body_bytes` storage cap
  also applies to captured AMQP units.
- The `Host` header is rewritten to the upstream, like any reverse proxy.
- A truncated capture cannot be replayed; the refusal is deliberate.
- GraphQL fragment spreads are not resolved: a top-level `...Fields` contributes
  no field names, because naming them would need the fragment and the schema.
- Captures taken before GraphQL support existed report no operation and no
  errors. Nothing is re-read retroactively: an old capture that quietly changed
  outcome is worse than one that is honestly blank.
- A statement prepared in one cycle and executed in a later one shows the name
  it was prepared under rather than its SQL — the text crossed the wire once, in
  the capture that holds the `Parse`, and nothing is written into a capture that
  did not cross it. The parameters, the timing and the outcome are all there,
  and drivers that prepare per query rather than per connection are unaffected.
- A client that pipelines more than sixteen statements without a single answer
  has the oldest written as it stands rather than held: nothing may pile up in
  memory waiting for an answer that is not coming.
- A Postgres connection that negotiates TLS is forwarded and captured, but the
  bytes after the handshake are TLS records and nothing in them can be read.
  Terminating it is a different job from terminating HTTPS: the client asks
  for encryption with an `SSLRequest` inside the protocol rather than before
  it, so Sonda would have to answer that byte itself, hold a TLS session in
  each direction, and pick the framing back up at the second startup message.
  `tls: true` is therefore refused on a postgres target rather than accepted
  and ignored.
- The interface has cursors but no trigger. A trigger — arm the field and let it
  capture when a named condition crosses — is the one device a real instrument
  has that this one still does not.
- No trace id of its own is injected. Requests that carry one are grouped
  exactly; the rest are grouped by nesting and the tree says it inferred them.

