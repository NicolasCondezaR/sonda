# Mirador

A capturing proxy for local development traffic. Point a client at Mirador
instead of the service it talks to, and every request and response that crosses
it becomes searchable.

It exists because debugging between services usually means reading logs from
several containers, none of which contain the payload. `mitmproxy` solves this
well for HTTP. Nothing solves it for gRPC — `grpcurl` and `grpcui` make calls,
they do not observe the ones your services make to each other. That gap is what
this is aimed at.

[![CI](https://github.com/NicolasCondezaR/mirador/actions/workflows/ci.yml/badge.svg)](https://github.com/NicolasCondezaR/mirador/actions/workflows/ci.yml)

*[Léeme en español](README.es.md).*

![The event field: one lane per service, faults as full-height bars](docs/assets/mirador-field.jpg)

> **Status: phase 6, feature complete.** Capture, decoding, storage, search,
> the query API, the web interface, replay, structural diff and a terminal
> client all work, and the whole thing runs from `docker compose up`. See
> [Roadmap](#roadmap).

## How it works

Mirador is an explicit proxy: one listening port per observed service. Nothing
is intercepted behind your back, and a service that is not configured is not
captured.

```
client ──▶ mirador :9101 ──▶ your service :3000
                │
                └──▶ SQLite ──▶ query API :9000
```

Two properties drive the design:

- **Forwarding is byte exact.** A debugger that alters the traffic invalidates
  every conclusion drawn from it. Request and response bodies pass through
  untouched, regardless of how much of them is stored.
- **The stored bytes are the record.** Bodies are saved exactly as they crossed
  the wire and decoded only when displayed. Re-serializing would lose unknown
  fields and reorder keys, which is what makes replay meaningless — and it lets
  a capture become readable later, once its schema is available.

## Quick start

### With Docker

```bash
docker compose up -d
```

This starts Mirador plus two toy upstreams to have something to capture: an
`echo` HTTP service and a `grpcdemo` gRPC service. Their own ports are
deliberately not published — traffic is meant to arrive through the proxy.

Open **http://127.0.0.1:9000** and send it something:

```bash
curl http://127.0.0.1:9101/ok                 # HTTP, through Mirador
curl 'http://127.0.0.1:9101/fail?status=503'  # a fault, to see one flagged
grpcurl -plaintext -d '{"order_id":"ORD-1"}' \
  127.0.0.1:9201 demo.v1.Orders/GetOrder      # gRPC, through Mirador
```

### Without Docker

```bash
go build -o mirador ./cmd/mirador
go build -o echo ./examples/echo
go build -o grpcdemo ./examples/grpcdemo

cp mirador.example.yaml mirador.yaml
./echo -addr 127.0.0.1:8081 &
./grpcdemo -addr 127.0.0.1:8082 &
./mirador -config mirador.yaml
```

## The interface

Mirador reads as a logic analyzer rather than a request table, because a table
answers "what happened" and never "what was happening at the same time" — which
is the question when fifteen services are talking and one of them broke.

- **Channel rail.** One row per target, carrying its colour from the logic-probe
  code, its total call count and its fault count. The counts are unfiltered: the
  rail answers "is this service healthy", so a filter in front of the field must
  not change it.
- **Event field.** One lane per channel against a live time axis whose right
  edge is now. A call is a mark whose width is its duration; **a fault is a
  different shape** — a full-height bar — so it survives a red channel, a
  colour-blind reader and a glance from across the room.
- **Inspector.** The selected call, decoded, with its schema source named.

It opens filtered to faults, because that is why you opened it. `ALL` switches
to the whole field. Resting the pointer in the field **holds the trace** so a
mark stops sliding while you aim at it; move away and it resumes.

`FIND` searches paths and payload text, including payloads Mirador only has as
bytes. `/` focuses it, `Escape` closes the inspector.

The whole interface is embedded in the binary — no Node, no build step, no
network requests, no webfonts.

![A gRPC failure: protobuf decoded through reflection, with the real status](docs/assets/mirador-grpc-inspector.jpg)

Above: a gRPC call that returned `PermissionDenied`. The HTTP status is 200 —
gRPC reports failure below HTTP — and the request is decoded to field names
because the service serves reflection.

## The terminal client

The same instrument, in a terminal. It is a second client of the API rather than
a second implementation: it captures nothing and stores nothing, it reads a
running Mirador.

```bash
go build -o mirador-tui ./cmd/mirador-tui
./mirador-tui                          # defaults to http://127.0.0.1:9000
./mirador-tui -api http://host:9000

docker compose run --rm tui            # or from the image
```

```
M I R A D O R  ■ LIVE   FAULTS  ALL    1M  5M  30M                  19 CAPTURED  ·  2 FLAGGED
CHANNEL       CALLS FAULT │-30M         -25M        -20M        -15M        -10M       -5M  NOW
 ■ echo       7     1     │·············│···········│···········│···········│·········█·····
▸■ orders     12    1     │·············│···········│···········│···········│·········█·····
──────────────────────────┴─────────────────────────────────────────────────────────────────
 POST /demo.v1.Orders/Fail
 orders   gRPC   HTTP 200   1.72ms
 gRPC 7 PermissionDenied — no tienes acceso a este pedido
 demo.v1.Orders / Fail   schema from reflection
 REQUEST  1 message(s)
   {
     "code": 7,
     "message": "no tienes acceso a este pedido"
   }
 RESPONSE  0 message(s)
 ↑↓ chan · ←→ call · ⏎ read · r replay · d diff · f faults · w window · h hold · / find · q quit
```

The translation is mostly direct — monospace is free here, hairlines become
box-drawing characters, and channel colours carry over unchanged. Two things
needed a different expression:

- There are no type sizes, so the four roles become weight and dimming.
- A lane is one row tall, so a fault cannot be a taller bar. It becomes a **full
  block where an ordinary call is a half one** (`█` against `▄`), with a third
  glyph for a cell holding both. Shape still carries the outcome before colour
  does, which is the rule that matters.

| Key | |
|---|---|
| `↑` `↓` | pick a channel |
| `←` `→` | step along it, call by call |
| `enter` | read the selected call |
| `r` | replay it |
| `d` | diff a replay against its original |
| `f` | faults only, or everything |
| `w` | cycle the sweep |
| `h` | hold the trace |
| `/` | search |
| `q` | quit |

Stepping moves call by call rather than cell by cell: an empty cell is not
something to point at. `h` holds the trace for the same reason the web client
freezes the field under the pointer — a mark that slides while you aim at it is
not selectable.

## PowerShell

PowerShell 5.1 rewrites quotes when passing arguments to external executables,
so `curl.exe` silently mangles JSON bodies:

```powershell
# WRONG — the upstream receives {sku:ABC-9}, quotes stripped
curl.exe -X POST -H "Content-Type: application/json" -d '{"sku":"ABC-9"}' http://127.0.0.1:9101/echo
```

Use `Invoke-RestMethod`, or put the body in a file:

```powershell
# Sends a body, and reads what Mirador captured
$body = @{ sku = 'ABC-9'; qty = 3 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:9101/echo -ContentType 'application/json' -Body $body

(Invoke-RestMethod -Uri http://127.0.0.1:9000/api/calls).calls |
  Select-Object id, method, path, status, duration_ms | Format-Table

$id = (Invoke-RestMethod -Uri 'http://127.0.0.1:9000/api/calls?q=ABC-9').calls[0].id
(Invoke-RestMethod -Uri "http://127.0.0.1:9000/api/calls/$id").request.text
```

```powershell
# curl.exe is fine when the body comes from a file
curl.exe -X POST -H "Content-Type: application/json" --data-binary '@body.json' http://127.0.0.1:9101/echo
```

## gRPC

Set `protocol: grpc` on a target and point it at the service's port. Mirador
speaks cleartext HTTP/2 to it, forwards the call untouched — trailers included,
which is where gRPC reports whether the call actually worked — and decodes the
messages when it can find a schema.

```bash
# through Mirador, not straight at the service
grpcurl -plaintext -d '{"order_id":"ORD-777"}' 127.0.0.1:9201 demo.v1.Orders/GetOrder

# only the calls that failed, across HTTP status, gRPC status and transport errors
curl 'http://127.0.0.1:9000/api/calls?failed=true'
```

### Where the schema comes from

Three sources, tried in order, each degrading into the next:

1. **Reflection.** If the service serves it, Mirador asks and needs nothing else.
   It asks the service directly rather than through the proxy, so its own
   bookkeeping does not end up in your timeline.
2. **A descriptor set on disk.** For services without reflection, compile the
   protos and point at the result:
   ```bash
   buf build -o descriptors.binpb
   # or: protoc --include_imports --descriptor_set_out=descriptors.binpb path/to/*.proto
   ```
   ```yaml
   - name: orders
     listen: 127.0.0.1:9201
     upstream: http://127.0.0.1:50051
     protocol: grpc
     descriptor_set: ./descriptors.binpb
     reflection: false
   ```
3. **The wire format itself.** With no schema at all, messages still come back
   as numbered fields with guessed types, nested structure intact:
   ```json
   [
     {"number": 1, "type": "string",  "value": "ORD-777"},
     {"number": 2, "type": "varint",  "value": 3, "note": "could be an integer, a bool or an enum"},
     {"number": 3, "type": "message", "value": [...], "note": "guessed to be a nested message"}
   ]
   ```
   Guesses are labelled as guesses. On the wire a varint really could be an
   int32, a bool or an enum, and saying so is the difference between a useful
   view and a misleading one.

`GET /api/schemas` reports which source each target resolved to, and the reason
when none did. That is the endpoint to check first when field names are missing.

To find out whether a service serves reflection at all:

```bash
grpcurl -plaintext <host>:<port> list
```

### What gRPC support does and does not cover

- Unary, server streaming and client streaming are all captured, with every
  message on both sides — not just the first.
- The status comes from the trailers, or from the headers on a trailers-only
  response, which is how a server reports an error with nothing to send. A
  failed gRPC call still carries HTTP 200; the listing shows both.
- `grpc-message` is percent-decoded, so an error in Spanish reads as Spanish.
- Compressed messages are reported as compressed and not decoded. The encoding
  is negotiated per call and guessing at it would produce confident garbage.
- A `grpc` target still forwards the plain HTTP that shares the port — health
  and metrics endpoints — and classifies it by content type rather than by how
  the target was configured.
- Reflection calls made *through* the proxy are captured like any other traffic.
  Filter them out with `?path=demo.v1` or whatever matches your own services.

## Replay and diff

Selecting a call in the inspector offers **REPLAY**. The request goes back out
built from the bytes that were stored, so what reaches the service is what
reached it the first time.

It is sent **through Mirador**, not straight at the upstream, which means the
replay is captured like any other traffic: it lands in the field, it is linked
to the call it came from, and the two can be compared immediately.

```bash
# replay a call onto the channel it came from
curl -X POST http://127.0.0.1:9000/api/calls/42/replay

# or onto another configured channel, to ask the same request of a second instance
curl -X POST http://127.0.0.1:9000/api/calls/42/replay \
  -H 'Content-Type: application/json' -d '{"target":"orders-staging"}'

curl 'http://127.0.0.1:9000/api/diff?a=42&b=43'
```

Replay only ever targets a **configured channel**. There is no arbitrary-URL
mode: the useful case is asking the same request of another instance you are
already observing, and anything wider turns a debugger into a request forge.

**A truncated capture cannot be replayed, and Mirador refuses rather than
trying.** Only the head of the body was stored, so what went out would not be
what was captured, and the result would carry the word "replay" while being a
different request. The refusal names the fix: raise `max_body_bytes` and capture
it again.

### The diff is structural

Bodies are compared as parsed structures, not as text. Reordered keys and
reindented blocks are not differences, so the answer is a short list of paths
instead of a wall of red and green:

```
~ qty          a 3          b 7
+ nota                      b urgente
```

- Array order **is** a difference — position carries meaning in a protobuf
  repeated field — while object key order is not.
- `1290000` and `"1290000"` are the same value. protojson renders int64 as a
  string, and reporting that would be a statement about encoding, not data.
- gRPC streams are compared message by message, so message 3 of 5 can differ
  while the rest match.
- Duration is deliberately excluded: it changes on every replay and would bury
  the differences that mean something.
- When a side is not JSON, or has no schema, the diff says so and reports
  whether the bytes match rather than inventing a structural comparison.

## What Mirador stores, and what that means

Mirador writes the bytes that crossed the wire into a SQLite file. **That
includes whatever your traffic carries**: `Authorization` headers, session
cookies, API keys, personal data. There is no redaction, and that is a
deliberate choice rather than an oversight — redacting would mean the capture is
no longer what was sent, which breaks both the fidelity the tool is built on and
replay along with it.

What follows from that:

- **The database is a plain file with no encryption**, wherever `database:`
  points. Treat it like a log file containing credentials, because it is one.
- **Mirador has no authentication.** Anyone who can reach its port can read
  every capture. Bind it to `127.0.0.1` — the shipped configuration does — and
  do not publish the port.
- **It is a local development tool.** Pointing it at production traffic is
  outside what it was built for and outside what its threat model covers.
- Retention bounds how long captures live, but it is a housekeeping limit, not a
  security control.

## Configuration

Copy `mirador.example.yaml` to `mirador.yaml` and add one entry per service.
Unknown keys are a startup error rather than a silent default, so a typo does
not turn into an hour of confusion.

```yaml
api_listen: 127.0.0.1:9000
database: mirador.db
max_body_bytes: 262144   # kept per body; the full body always reaches its destination
buffer_size: 1024        # captures buffered in memory before they are written

retention:
  max_calls: 50000
  max_age: 24h
  interval: 1m

targets:
  - name: admin-api
    listen: 127.0.0.1:9102
    upstream: http://127.0.0.1:3000
    protocol: http
```

Then point whatever calls `admin-api` at `127.0.0.1:9102`. The same binary and
the same file work for services in containers and for services running natively
— which is the point, since a real local stack is usually both.

Inside Docker, use `host.docker.internal` to reach a service running on the
host. See `mirador.docker.yaml`.

## API

| Method and path | Purpose |
|---|---|
| `GET /api/calls` | List captures, newest first. Filters: `target`, `method`, `path`, `status`, `protocol`, `grpc_status`, `failed`, `q`, `since`, `until`, `limit`, `before_id`. |
| `GET /api/calls/{id}` | One capture with headers and bodies. |
| `GET /api/targets` | The configured targets. |
| `GET /api/schemas` | Per gRPC target: which schema source resolved, or why none did. |
| `POST /api/calls/{id}/replay` | Send the call again, optionally onto another channel. |
| `GET /api/diff?a=&b=` | Structural comparison of two calls. |
| `GET /api/stats` | Capture count, time span, and calls dropped under load. |
| `GET /health` | Liveness. |

The listing deliberately carries no bodies — a few hundred calls with payloads
attached is unusable. Bodies come from the detail endpoint, as `text` when the
content is valid UTF-8 and as `base64` when it is not. The API never guesses:
it reports what the bytes are.

`q` searches paths and text payloads. It is treated as a literal phrase, so
`"sku":"ABC-9"` and `/v1/orders` work as typed instead of being read as query
operators.

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
- **An unreachable upstream is captured too.** Mirador answers 502 and records
  the transport error, so the failure is in the timeline rather than missing
  from it. A 502 that came from the upstream itself has an empty `error` field.
- **Retention runs on a timer**, applying age first and then the row cap.
- **Ctrl+C drains the buffer** before exiting.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1 | HTTP capture, storage, search, query API | done |
| 2 | gRPC: h2c, trailers, message framing, protobuf decoding | done |
| 3 | Web UI with a live timeline | done |
| 4 | Replay and structural diff | done |
| 5 | Packaging and documentation | done |
| 6 | TUI, as a second client of the same API | done |

### Limitations

- Plaintext only. A TLS upstream is forwarded but cannot be inspected.
- No WebSocket or SSE capture.
- Compressed gRPC messages are not decompressed.
- The `Host` header is rewritten to the upstream, like any reverse proxy.
- A truncated capture cannot be replayed; the refusal is deliberate.
- The interface has no cursors and no trigger — two devices a real instrument
  has, and the obvious next reach.

## Development

```bash
go test ./...
go vet ./...
go run ./cmd/mirador -version   # the commit a binary came from
```

The race detector needs cgo, and this project deliberately needs no C toolchain
(the SQLite driver is pure Go), so `go test -race` runs in CI rather than on a
Windows workstation. It is not a formality: the proxy reads a request body on
the transport's goroutine while the handler reads the capture.

Tests use real SQLite files, real HTTP servers and a real gRPC client and
server; there are no mocks of any of them.

`PRODUCT.md` and `DESIGN.md` hold the product record and the visual system. The
interface is plain HTML, CSS and JavaScript under `internal/web/static`, served
through `go:embed`; editing it needs no toolchain, only a rebuild.

After changing `examples/grpcdemo/proto`, regenerate the Go code and the
committed descriptor set:

```bash
go install github.com/bufbuild/buf/cmd/buf@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

buf lint
buf generate
buf build -o examples/grpcdemo/descriptors.binpb
```

`buf` is a pure-Go compiler, so no `protoc` install is needed. A test fails if
the committed descriptor set drifts from the generated code.
