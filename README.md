# Sonda

A capturing proxy for local development traffic. Point a client at Sonda
instead of the service it talks to, and every request and response that crosses
it becomes searchable — arranged into the request it belonged to, replayable,
comparable, and readable by a coding agent as well as by you.

It exists because debugging between services usually means reading logs from
several containers, none of which contain the payload. `mitmproxy` solves this
well for HTTP. Nothing solves it for gRPC — `grpcurl` and `grpcui` make calls,
they do not observe the ones your services make to each other. That gap is what
this is aimed at.

[![CI](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml/badge.svg)](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml)

*[Léeme en español](README.es.md).*

![The event field: one lane per service, faults as full-height bars](docs/assets/sonda-field.jpg)

> **Status: phase 17.** Capture, decoding, storage, search, the query API, the
> web interface, replay, structural diff, a terminal client, project management,
> [request trees](#agents), [stub mode](#stub-mode) and an
> [MCP server for coding agents](#agents) all work, and the whole thing runs
> from `docker compose up`. See [Roadmap](#roadmap).

## How it works

Sonda is an explicit proxy: one listening port per observed service. Nothing
is intercepted behind your back, and a service that is not configured is not
captured.

```
client ──▶ sonda :9101 ──▶ your service :3000
                │
                └──▶ SQLite ──▶ query API :9000
```

Two properties drive the design:

- **Forwarding is byte exact.** A debugger that alters the traffic invalidates
  every conclusion drawn from it. Request and response bodies pass through
  untouched, regardless of how much of them is stored — including a WebSocket
  handshake, whose key and negotiated extensions are relayed verbatim so the two
  ends agree with each other and not with Sonda.
- **The stored bytes are the record.** Bodies are saved exactly as they crossed
  the wire and decoded only when displayed. Re-serializing would lose unknown
  fields and reorder keys, which is what makes replay meaningless — and it lets
  a capture become readable later, once its schema is available. The single
  exception is stated where it applies: a [Postgres](#postgresql) password is
  blanked in the capture before it is written, because a credential in a
  plaintext file cannot be taken back out afterwards.

## Install

Pick whichever you already have. Nothing here needs a C toolchain or a system
SQLite: the driver is pure Go, which is also why the binaries are static and the
image is 50 MB.

| | |
|---|---|
| **macOS** | `brew install NicolasCondezaR/tap/sonda` |
| **Windows** | `scoop bucket add nicolascondezar https://github.com/NicolasCondezaR/scoop-bucket`<br>`scoop install sonda` |
| **Go** | `go install github.com/NicolasCondezaR/sonda/cmd/sonda@latest` |
| **Docker** | `docker run -p 9000:9000 -v sonda:/data ghcr.io/nicolascondezar/sonda` |
| **Binary** | Download from [Releases](https://github.com/NicolasCondezaR/sonda/releases), unpack, run |
| **Source** | `git clone` and `go build ./cmd/sonda` |

On Linux, use `go install`, the image, or the tarball: Homebrew casks are macOS
only.

The release archives carry four binaries, not one: `sonda`, the terminal client
`sonda-tui`, and the two toy services `echo` and `grpcdemo` that the quick start
below uses — so there is something to capture without wiring up your own. The
package managers install only the first two, since a binary named `echo` on the
PATH has no business shadowing the system one.

```bash
sonda            # the proxy and the interface, on http://127.0.0.1:9000
sonda -version   # which build this is
sonda-tui        # the terminal client
```

Downloading the tarball by hand on macOS has one extra step: Gatekeeper
quarantines anything unsigned that arrives through a browser. Either fetch it
with `curl`, or clear the flag once. Homebrew does this for you.

```bash
xattr -dr com.apple.quarantine sonda sonda-tui echo grpcdemo
```

## Quick start

### With Docker

```bash
docker compose up -d
```

This starts Sonda plus two toy upstreams to have something to capture: an
`echo` HTTP service and a `grpcdemo` gRPC service. Their own ports are
deliberately not published — traffic is meant to arrive through the proxy.

Open **http://127.0.0.1:9000** and send it something:

```bash
curl http://127.0.0.1:9101/ok                 # HTTP, through Sonda
curl 'http://127.0.0.1:9101/fail?status=503'  # a fault, to see one flagged
grpcurl -plaintext -d '{"order_id":"ORD-1"}' \
  127.0.0.1:9201 demo.v1.Orders/GetOrder      # gRPC, through Sonda
```

### Without Docker

```bash
go build -o sonda ./cmd/sonda
go build -o echo ./examples/echo
go build -o grpcdemo ./examples/grpcdemo

cp sonda.example.yaml sonda.yaml
./echo -addr 127.0.0.1:8081 &
./grpcdemo -addr 127.0.0.1:8082 &
./sonda -config sonda.yaml
```

## The interface

Sonda reads as a logic analyzer rather than a request table, because a table
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

`FIND` searches paths and payload text, including payloads Sonda only has as
bytes. `/` focuses it, `Escape` closes the inspector.

The whole interface is embedded in the binary — no Node, no build step, no
network requests, no webfonts.

![A gRPC failure: protobuf decoded through reflection, with the real status](docs/assets/sonda-grpc-inspector.jpg)

Above: a gRPC call that returned `PermissionDenied`. The HTTP status is 200 —
gRPC reports failure below HTTP — and the request is decoded to field names
because the service serves reflection.

## Projects

A project groups the services of one system — a monorepo, a side project,
whatever is being worked on today — and everything about it is configured from
the interface. Press **PROJECTS**.

The grouping is not filing for its own sake. It carries the two things that are
shared across a system's services and would otherwise be repeated on each one:

- **One descriptor set for the whole project.** Uploaded, not referenced by
  path, so it travels with the database when it is copied to another machine.
- **One answer to "are these ports open".** Only the active project listens, so
  two projects can claim the same port without colliding, and switching closes
  one set and opens the other without restarting anything.

Captures are tagged with the project they were taken under, so switching does
not pour one system's traffic into another's field.

### Import instead of typing

Setting up fifteen services by hand is how a tool like this gets abandoned after
one afternoon. The addresses are already written down somewhere, so
**IMPORT FROM A FILE** reads them: a `.env` full of `*_URL` entries, or a
compose file with published ports.

Every entry comes back with the line it was found on, and with its suggested
port already probed, so a wrong reading or a clash is visible before anything is
saved. Nothing is added until you say so.

```
+  ms-auth       grpc  http://localhost:50052  127.0.0.1:9152  port already in use
+  ms-billing    grpc  http://localhost:50067  127.0.0.1:9167  line 3: MS_BILLING_GRPC_URL
+  ms-executive  grpc  http://localhost:50064  127.0.0.1:9164  line 2: MS_EXECUTIVE_GRPC_URL
```

Database URLs, message brokers, callback URLs and anything else that is not a
service to call are left out. A list with a connection string in it is worse
than a list missing an entry: the first gets saved and proxied, the second gets
noticed.

### The one step no screen removes

Sonda is an explicit proxy. It sees nothing until whoever makes the call is
told to call it instead — no amount of configuration screen changes that, because
the caller decides where its requests go.

So each service hands over the exact line, ready to copy:

```
point the caller here:  MS_AUTH_GRPC_URL=127.0.0.1:9152
```

Restart the caller with that in its environment and its traffic appears in the
field. Nothing on disk changes, and dropping the variable puts it back.

### The configuration file

`sonda.yaml` still carries the process-level settings — where the API listens,
how much of a body to keep, how long captures live. Its `targets` are only a
**seed**: they become the first project the first time a database is created,
and are ignored afterwards, so an edit made in the interface is never undone by
a stale file. Running with no configuration file at all is an ordinary first
run.

## The terminal client

The same instrument, in a terminal. It is a second client of the API rather than
a second implementation: it captures nothing and stores nothing, it reads a
running Sonda.

```bash
go build -o sonda-tui ./cmd/sonda-tui
./sonda-tui                          # defaults to http://127.0.0.1:9000
./sonda-tui -api http://host:9000

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
 ↑↓ chan · ←→ call · ⏎ read · t tree · c contract · r replay · d diff · f faults · w window · h hold · / find · q quit
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
| `t` | show the whole request it belonged to, as a tree |
| `c` | has this endpoint changed shape since it used to work |
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
# Sends a body, and reads what Sonda captured
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

Set `protocol: grpc` on a target and point it at the service's port. Sonda
speaks cleartext HTTP/2 to it, forwards the call untouched — trailers included,
which is where gRPC reports whether the call actually worked — and decodes the
messages when it can find a schema.

```bash
# through Sonda, not straight at the service
grpcurl -plaintext -d '{"order_id":"ORD-777"}' 127.0.0.1:9201 demo.v1.Orders/GetOrder

# only the calls that failed, across HTTP status, gRPC status and transport errors
curl 'http://127.0.0.1:9000/api/calls?failed=true'
```

### Where the schema comes from

Three sources, tried in order, each degrading into the next:

1. **Reflection.** If the service serves it, Sonda asks and needs nothing else.
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

It is sent **through Sonda**, not straight at the upstream, which means the
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

**A truncated capture cannot be replayed, and Sonda refuses rather than
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

## What Sonda stores, and what that means

Sonda writes the bytes that crossed the wire into a SQLite file. **That
includes whatever your traffic carries**: `Authorization` headers, session
cookies, API keys, personal data. Nothing is redacted on the way in, and that is
a deliberate choice rather than an oversight — redacting the capture would mean
it is no longer what was sent, which breaks both the fidelity the tool is built
on and replay along with it.

There is exactly one exception, and it is one because the trade-off runs the
other way: a **Postgres** password is blanked in the capture as the bytes go
past. A database capture cannot be replayed anyway, so nothing is lost by not
keeping it, and the alternative is a live credential in a plaintext file. See
[PostgreSQL](#postgresql).

The other place credentials are held back is [the MCP server](#agents), because
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

## Agents

Sonda speaks the [Model Context Protocol](https://modelcontextprotocol.io), so a
coding agent can read the captures itself instead of being told about them.

The loop it replaces is the tedious one: agent writes code, you run it, you copy
a log, you paste it back, the agent guesses. With this, the agent runs the code
and then asks what actually crossed the wire — decoded, and not filtered through
whatever somebody chose to log.

### Connecting

Two ways in, same server behind both.

**A URL**, if your client accepts one. Nothing to install, and several agents
pointed at the same Sonda see the same captures:

```
http://127.0.0.1:9000/mcp
```

**A command**, for clients that only speak over a pipe. It forwards to the Sonda
that is already running, so it is still the same data:

```json
{
  "mcpServers": {
    "sonda": { "command": "sonda", "args": ["mcp"] }
  }
}
```

`sonda mcp --api http://127.0.0.1:9000` points it somewhere else.

### The tools

| Tool | What it answers |
|---|---|
| `recent_failures` | "What just broke?" — the first question, usually |
| `search_calls` | By service, method, path, status, or text in the bodies |
| `get_call` | One call in full, decoded |
| `diff_calls` | "This one worked and this one did not — what changed?" |
| `trace_call` | Every call that was part of the same request, as a tree |
| `list_services` | What is being observed, on which ports, and whether it is listening |
| `wait_for_call` | Blocks until matching traffic appears. Trigger something, then verify it |
| `replay_call` | Send a capture again. Marked destructive, so clients ask first |
| `connect_project` | Set Sonda up to watch a whole system, and hand back the edit that makes traffic flow through it |
| `configure_service` | Add or fix one service |
| `activate_project` | Open the ports. Asks first |
| `disconnect_project` | Close them and hand back the edit that undoes the pointing. Asks first |
| `set_stub` | Answer for a service from recordings instead of forwarding. Asks first |
| `break_service` | Add latency, force a status, or cut the connection. Asks first |
| `contract_drift` | Has this response changed shape since it used to work |

`wait_for_call` is the one that turns Sonda into a check rather than a viewer:
the agent makes a change, triggers the action, and waits for what should have
gone over the wire. Nothing arriving is also an answer.

### Connecting a project by asking

> *"Connect the monorepo to Sonda."*

The agent reads the project's own `.env` or compose file and hands the contents
to `connect_project`. Sonda finds the services, assigns a proxy port to each,
creates the project — and returns the exact edit to apply:

```json
{
  "project": "core-delpagroup",
  "services": 21,
  "active": false,
  "changes": {
    "MS_AUTH_GRPC_URL":  { "from": "localhost:50052", "to": "127.0.0.1:9152" },
    "MS_ADMIN_GRPC_URL": { "from": "localhost:50053", "to": "127.0.0.1:9153" }
  }
}
```

That last part is the whole design. **Sonda cannot repoint a caller** — it is an
explicit proxy, and it sees nothing until something is told to talk to it. The
agent can: it has the filesystem and it can restart a process. So Sonda knows
the mapping and the agent has the hands.

`disconnect_project` returns the inverse. Without it, an agent that repointed a
`.env` and then stopped would leave the environment aimed at ports nobody is
listening on.

Creating configuration disturbs nobody, so those tools run freely. Opening and
closing ports can pull the floor out from under you mid-debug, so those ask
first.

### Credentials do not leave

Everything above is filtered before it goes out. `Authorization`, `Cookie`,
`X-Api-Key`, `password`, `client_secret` and their various spellings come back
as `[redacted by Sonda]` — in headers, in bodies, and inside JSON nested in a
body. **There is no setting to turn this off**, deliberately: a flag for it
would be switched on against a toy project and then forgotten against a real
one. The web interface still shows everything, because there the reader is you.

Bodies are also shortened by default; `get_call` takes `detail` for the whole
thing. `detail` does not reveal credentials — that is covered by a test.

The HTTP endpoint refuses requests carrying a foreign `Origin`, which is what
stops a page in your own browser from reaching it through DNS rebinding and
reading your captures.

## Sockets and event streams

A WebSocket stops being requests and responses the moment its handshake
succeeds, so Sonda holds both directions of it itself rather than letting the
reverse proxy relay bytes nobody sees. What is stored is the raw frame stream
per direction, and the frames are read back out when you look — the same
arrangement gRPC streaming already uses.

- The handshake goes through **verbatim**: the key, the subprotocols and the
  negotiated extensions are what the two ends are agreeing on, and changing any
  of them would make the recorded conversation a different one.
- Client frames are **unmasked** for display. The mask is transport scaffolding
  the receiving end removes before anything reads the payload; the key is kept
  so the frame can still be reproduced exactly.
- Text, binary, ping, pong, fragments and close all show as what they are, and a
  close frame reports its code and reason — usually the answer to why the socket
  stopped working.
- An upgrade the service **refuses** is relayed as the ordinary response it is,
  so you can read why.

**A socket is captured when it closes**, not while it is open: it is one
exchange, and the exchange is not over. A long-lived socket is bounded by the
same body cap as any other capture, and says how much it did not keep.

Server-sent events need none of that — the response is ordinary HTTP — so they
are captured as they always were and split back into their events for display,
comments and keepalives dropped, a partial last event shown as partial.

## GraphQL

A GraphQL client sends every operation as the same POST to the same path, so a
service that speaks GraphQL arrives in the field as one call repeated a hundred
times and the timeline stops being useful for it. Sonda reads the operation out
of the body and labels the call with it: `POST /graphql · mutation Pay`.

- **The document is detected by its body, not its path.** A POST whose JSON
  carries a `query` string is a GraphQL request wherever it was sent — behind a
  gateway prefix, on `/api`, or on `/graphql`.
- **A batch is every operation in it.** Clients send arrays of operations as one
  request; reading only the first would hide the rest.
- **The inspector shows the operation**: its type and name, the top-level fields
  it asked for, the variables it sent, and every error the response carried with
  its path and its `extensions.code`. The raw bodies stay alongside it.

**A GraphQL error is a failure.** The server answers HTTP 200 with an `errors`
array, so a tool that reads the status code alone shows the thing you came to
find as a success. Sonda counts it as a fault everywhere the question is asked:
the fault filter, the channel rail's counts, the field's fault marks, the
terminal, the trace tree and `recent_failures` over MCP. This is the same
problem gRPC has, and it is answered the same way.

Sonda reads enough of the document to name the operation and no more. It is not
a parser: it does not validate, resolve fragment spreads into their fields, or
know your schema. A response that is not JSON — cut by the body cap, or an error
page from something in front of the service — is reported as unreadable rather
than as a call with no errors.

## PostgreSQL

Every other protocol Sonda captures is one hop between services. Postgres is the
hop underneath: it puts the SQL a request actually ran in the field under the
HTTP call that caused it, one row per statement, and an N+1 stops being a theory
and becomes forty rows under one handler.

A database target is declared like any other service, with `protocol: postgres`
and an upstream that names the transport:

```yaml
  - name: orders-db
    listen: 127.0.0.1:9301
    upstream: postgres://127.0.0.1:5432
    protocol: postgres
```

Then point the application's DSN at the listen address, keeping its own user,
password and database name:

```
DATABASE_URL=postgres://app:secret@127.0.0.1:9301/orders
```

**The upstream carries no credentials, and Sonda refuses one that does.** It
forwards the client's own handshake untouched, so it has no use for a user or a
password — and a password in the configuration would be a password written into
`sonda.db`.

### The password never reaches the capture

This is the one place Sonda rewrites what it keeps, and the reason is that the
alternative cannot be fixed later. A startup exchange carries the password. If
the raw bytes were stored as they arrived, the secret would sit in `sonda.db` in
plaintext and could reach an agent through MCP — whose redaction walks headers
and JSON keys, which a TCP stream is neither.

So the credential bytes are blanked in the tap, as they go past, before anything
is stored: the PasswordMessage and SASL response bodies, the server's
authentication challenges, and the cancellation key in both directions. They are
replaced by filler of the same length, so every length field in the stream stays
true and the capture still reads back as a conversation. What survives is that
an authentication happened and by which mechanism — `sasl`, `md5_password`,
`cleartext_password` — which is the part a reader is entitled to.

**What is forwarded is untouched.** The rewrite applies to the copy Sonda keeps,
never to the bytes the database receives; the real password reaches the server
and the login works. Both halves of that are covered by tests.

### What a capture is

- **One capture is one statement, not one connection.** The protocol gives the
  boundary away: a simple query is `Q` → results → `ReadyForQuery`, an extended
  one is Parse/Bind/Describe/Execute/Sync → results → `ReadyForQuery`, and the
  `Z` ends the cycle in both. Each row carries the SQL, the values bound to it,
  what the server answered — the command tag, the row count or the error — and
  the timing of that statement alone.
- **The SQL hangs under the request that ran it**, and nothing new correlates
  them. Sonda already arranges calls into a tree by containment: a call that
  begins no earlier and ends no later than another is its child, and a statement
  run during an HTTP request is contained in it by definition. That only works
  because a capture is timed from the statement rather than from the connection,
  which is the whole reason the split exists — behind a pool one connection
  carries hours of an application's SQL, and hours of SQL is not something you
  can hang off a request.
- **The connection's opening rides its first statement.** The startup
  parameters, the mechanism that was demanded, the server's settings: they
  happen once, and they are context for the first thing the connection ran
  rather than a row of their own — a row with no SQL in it would be noise on
  every pooled connection. Dropping the fact that an authentication happened is
  not something a debugger gets to do, so a connection that authenticated and
  then ran nothing does get its own row, with the method `SESSION`.
- **The path is the database**, read off the startup message rather than
  invented, and carried onto every statement of the connection — only the first
  one holds the message it came from. The method is `STATEMENT`. There is no
  HTTP status, and none is shown.
- **The listing carries the statement itself** and how it ended
  (`SELECT id FROM orders WHERE total > 100 -> SELECT 12`), or the error if
  there was one. Every result of a cycle is named, not just the first: a `COPY`
  and a multi-statement simple query both answer once with several.
- **A statement that never finished is recorded and says so.** The connection
  died mid-query, or the capture ended while the statement was still in flight:
  the row is written with what crossed and an error saying no `ReadyForQuery`
  ever arrived. Reporting it as a success would be a lie, and dropping it would
  lose exactly the statement worth having.
- **The statement is searchable in full** — the SQL, the values bound to it, the
  command tags and the server's complaint — not only the summary line.
- **The inspector shows the messages**: the statements, their bind parameters
  with `NULL` distinguished from the empty string, the columns described, the
  command tags, the transaction status, and any server error with its SQLSTATE,
  detail and hint. Data rows are counted rather than drawn one by one.

**A SQL error is a failure.** It arrives as an `ErrorResponse` inside the stream
with no status code anywhere, so a tool that reads transports alone would show
it as a healthy statement. Sonda counts it as a fault everywhere the question is
asked: the fault filter, the channel rail, the field's fault marks, the terminal,
the trace tree and `recent_failures` over MCP. It is the same problem gRPC and
GraphQL have, answered the same way.

**A statement cannot be replayed.** It belongs to a connection, a session and a
transaction that are gone, and sending the SQL again would run it somewhere
else. Every surface refuses, including the API itself, so an agent gets the same
answer the browser does.

## Contract drift

In a monorepo where nobody versions a contract, a field that quietly went away
or changed type breaks the caller days later, far from the change that caused
it. Sonda already holds every response a service ever gave.

```
CONTRACT                                vs capture #412
−  data.items[].currency                    was string
~  data.total                          number -> string
+  data.meta.cached                              boolean

2 of these would break a caller.
```

It compares **shapes, not values**. Two calls returning different prices are not
drift; one returning a price as a number and the other as a string is.

- The baseline is the **oldest capture Sonda holds** of the same endpoint, not a
  schema someone has to maintain — a baseline nobody keeps up to date is a
  baseline that is gone in three weeks.
- A list collapses to the shape of its items. Two hundred orders report the
  shape of one order, or the field that changed would be buried under itself.
- A **nullable field is not drift.** Flagging every one of them buries the
  changes that matter under noise nobody can act on.
- An empty list claims nothing about what it holds. Guessing would invent a
  contract nobody wrote.
- **Adding a field is safe**; losing one or changing its type is what takes a
  caller down, and the report says which is which.

In the interface it is a section of the inspector, in the terminal it is `c`,
and for an agent it is `contract_drift`. This is the one thing in Sonda that
never touches the proxy: it only reads what was already stored.

## Breaking things on purpose

Retry logic, timeouts and degradation are written once and then never
exercised, because making a real service fail on demand is awkward enough that
nobody does it. Sonda is already in the path of every call.

```bash
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-rates","latency_ms":2000}'
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-seo","status":503,"one_in":3}'
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-auth","cut":true}'
```

From an agent, `break_service` does the same, and it asks first.

**Latency lets the call through** — the service still answers, it just takes
longer, which is the case a timeout is meant to catch. **A status or a cut ends
the call at Sonda**: the service is never reached.

### Deterministic, not random

`one_in: 3` means one call in every three, in that order, every run. A
percentage would behave differently each time and turn a failing test into a
coin toss; a sequence you can reproduce is the only kind worth debugging
against. Changing a rule restarts its schedule.

### It can never pass for a real failure

Every injected failure carries **`X-Sonda-Fault`** with the reason, is recorded
as injected, and is marked as such in the field, the inspector and the terminal.
The channel shows **BROKEN** while a rule is in force. Rules are forgotten when
Sonda restarts, for the same reason stubbing is: a service that has been failing
since Tuesday because of a rule nobody remembers setting is a worse afternoon
than the bug being chased.

## Stub mode

Sonda already holds the exact bytes of every response a service ever gave. Handing
them back instead of forwarding turns the same tool into something else:

- Work on the front while its backend is down
- Run a test without twenty-one processes
- Reproduce a bug from a capture, on a laptop, without the environment that made it

```bash
curl -X POST http://127.0.0.1:9000/api/stub   -d '{"service":"ms-rates","enabled":true}'
```

From an agent, `set_stub` does the same. It asks first — a service quietly
answering from last week is exactly the kind of surprise worth confirming.

### Why it cannot be mistaken for the real thing

A recorded answer that passes for a live one is the failure this feature has to
avoid, so four things make that hard to do by accident:

- Every stubbed response carries **`X-Sonda-Stub: <call id>`**
- The exchange is still captured, linked back to the recording it came from, so
  the field never shows traffic that never happened as though it had
- **Stubbing is forgotten when Sonda restarts.** It is never written to the
  database: a stub that outlives a restart is one nobody remembers turning on
- A request with no recording gets a **501 that explains itself**, not an
  invented answer and not a silent empty 200

### Which recording answers

An identical request body wins outright — that is the difference between
replaying *the answer to GetOrder* and *the answer to GetOrder(ORD-1)*, and a
test handed somebody else's order is worse off than one handed an error. Failing
that, the most recent call to the same method and path.

Captures that were themselves stubbed are never reused. Without that, leaving
stubbing on would slowly feed Sonda its own answers.

gRPC works too: the recorded trailers are replayed, so the client gets the real
`grpc-status` instead of waiting for one that never comes.

## Configuration

Copy `sonda.example.yaml` to `sonda.yaml` and add one entry per service.
Unknown keys are a startup error rather than a silent default, so a typo does
not turn into an hour of confusion.

```yaml
api_listen: 127.0.0.1:9000
database: sonda.db
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
    protocol: http     # http, grpc or postgres
```

Then point whatever calls `admin-api` at `127.0.0.1:9102`. The same binary and
the same file work for services in containers and for services running natively
— which is the point, since a real local stack is usually both.

Inside Docker, use `host.docker.internal` to reach a service running on the
host. See `sonda.docker.yaml`.

## API

| Method and path | Purpose |
|---|---|
| `GET /api/calls` | List captures, newest first. Filters: `target`, `method`, `path`, `status`, `protocol`, `grpc_status`, `failed`, `q`, `since`, `until`, `limit`, `before_id`. |
| `GET /api/calls/{id}` | One capture with headers and bodies. |
| `GET /api/targets` | The configured targets. |
| `GET /api/schemas` | Per gRPC target: which schema source resolved, or why none did. |
| `POST /api/calls/{id}/replay` | Send the call again, optionally onto another channel. |
| `GET /api/diff?a=&b=` | Structural comparison of two calls. |
| `GET /api/trace?call=` | The whole request a call belonged to, as a tree. |
| `GET /api/stub` | Which services are answering from recordings. |
| `POST /api/stub` | Turn stubbing on or off for a service, or clear it. |
| `GET /api/faults` | Which services are being broken on purpose, and how. |
| `GET /api/drift?target=` | Whether an endpoint still answers the shape it used to. |
| `POST /api/faults` | Set or clear a fault rule. |
| `GET /api/projects` | Projects, their services, and what is really listening. |
| `POST /api/projects` | Create one. `PATCH`/`DELETE /api/projects/{id}` rename and remove. |
| `POST /api/projects/{id}/activate` | Close the current project's ports and open this one's. |
| `POST /api/projects/deactivate` | Close every port. Nothing is deleted and activating brings it all back. |
| `POST /api/projects/{id}/descriptor` | Upload the compiled schemas for the whole project. |
| `POST /api/projects/{id}/services` | Add or update a service. `DELETE /api/services/{id}` removes one. |
| `POST /api/discover` | Read services out of a `.env` or compose file without saving anything. |
| `GET /api/runtime` | Which project is active and what is really listening. |
| `GET /api/stats` | Capture count, time span, and calls dropped under load. |
| `GET /api/stream` | Server-sent events: every capture the moment it is stored. What the live field reads. |
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
- **An unreachable upstream is captured too.** Sonda answers 502 and records
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
| 7 | Projects: grouped services, configured from the interface, imported from a file | done |
| 8 | MCP server, so a coding agent reads the captures itself | done |
| 9 | Configuration over MCP: connect a whole project by asking | done |
| 10 | Correlation: the calls of one request, arranged as a tree | done |
| 11 | Stub mode: answer from a recording instead of forwarding | done |
| 12 | The tree and the stub, on every surface: web, terminal, MCP | done |
| 13 | WebSocket and server-sent events | done |
| 14 | Fault injection: latency, forced statuses, cut connections | done |
| 15 | Contract drift: a field gone, a field retyped | done |
| 16 | GraphQL: the operation behind every identical POST, and its errors counted as failures | done |
| 17 | PostgreSQL: one capture per statement, hung under the request that ran it, with the credentials blanked before they are stored | done |

### Limitations

- Plaintext only. A TLS upstream is forwarded but cannot be inspected.
- Compressed gRPC messages are not decompressed.
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
  bytes after the handshake are TLS records and nothing in them can be read —
  the same limit as any other encrypted upstream.
- The interface has no cursors and no trigger — two devices a real instrument
  has, and the obvious next reach.
- No trace id of its own is injected. Requests that carry one are grouped
  exactly; the rest are grouped by nesting and the tree says it inferred them.

## Development

```bash
go test ./...
go vet ./...
go run ./cmd/sonda -version   # the commit a binary came from
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
