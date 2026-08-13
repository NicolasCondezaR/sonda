# Sonda

A capturing proxy for local development traffic. Point a client at Sonda
instead of the service it talks to, and the HTTP calls, streams, database
statements and AMQP units that cross it become searchable, comparable where
that is meaningful, and readable by a coding agent as well as by you.

It exists because debugging between services usually means reading logs from
several containers, none of which contain the payload. `mitmproxy` solves this
well for HTTP. Nothing solves it for gRPC — `grpcurl` and `grpcui` make calls,
they do not observe the ones your services make to each other. That gap is what
this is aimed at.

[![CI](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml/badge.svg)](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml)

*[Léeme en español](README.es.md).*

![The event field: one lane per service, faults as full-height bars](docs/assets/sonda-field.jpg)

> **Status: phase 19 complete.** Capture, decoding, storage, search, the query API, the
> web interface, replay, structural diff, a terminal client, project management,
> [request trees](#agents), [stub mode](#stub-mode), an
> [MCP server for coding agents](#agents) and [TLS](#tls) all work, and the whole
> thing runs from `docker compose up`. [AMQP 0-9-1](#amqp-0-9-1) and AMQPS now
> run through the same capture, storage and inspection surfaces. See
> [Roadmap](#roadmap).

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
  exceptions are stated where they apply: [Postgres](#postgresql) passwords and
  [AMQP](#amqp-0-9-1) SASL challenges and responses are blanked in the capture
  before it is written, because a credential in a plaintext file cannot be
  taken back out afterwards.

## Install

Pick whichever you already have. Nothing here needs a C toolchain or a system
SQLite: the driver is pure Go, which is also why the binaries are static and the
image is 50 MB.

| | |
|---|---|
| **macOS** | `brew install NicolasCondezaR/tap/sonda` |
| **Windows** | `scoop bucket add nicolascondezar https://github.com/NicolasCondezaR/scoop-bucket`<br>`scoop install sonda` |
| **Go** | `go install github.com/NicolasCondezaR/sonda/cmd/sonda@latest` |
| **Docker** | `docker run -p 127.0.0.1:9000:9000 -v sonda:/data ghcr.io/nicolascondezar/sonda` |
| **Binary** | Download from [Releases](https://github.com/NicolasCondezaR/sonda/releases), unpack, run |
| **Source** | `git clone` and `go build ./cmd/sonda` |

On Linux, use `go install`, the image, or the tarball: Homebrew casks are macOS
only.

The Docker line publishes to `127.0.0.1` rather than to every interface, and
every other one here does the same: `sonda.db` holds whatever credentials
crossed the wire, in plaintext and behind no login. Inside the container Sonda
binds `0.0.0.0` — the isolation is the container's job — which is what
`-api-listen` in the image's command does; outside one, the default stays
loopback.

That Docker line publishes the interface and nothing else, which is enough to
look around and not enough to capture anything: **a proxy needs its own
published port per service**, because the port a client connects to is the
whole mechanism. Adding a service on `9101` and then finding that nothing on
your machine can reach it is the confusing half hour this paragraph exists to
prevent.

```bash
docker run -p 127.0.0.1:9000:9000 -p 127.0.0.1:9101:9101 \
  -v sonda:/data ghcr.io/nicolascondezar/sonda
```

`docker compose up` publishes 9101 and 9201 already, which is why the quick
start below works without saying any of this.

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

### Start Sonda when you sign in to Windows

On Windows, Sonda can install one Task Scheduler entry for the current user.
It runs at ordinary user privilege, only after that user signs in, and stores
no password:

```powershell
sonda autostart install -config "$HOME\.sonda\sonda.yaml"
sonda autostart status
```

`install` creates the task, or reuses it only when its complete definition still
matches the current user and configuration. It never replaces a foreign or
modified task at Sonda's deterministic name. It starts the verified task
immediately and waits for `/health`; no restart is needed to check the setup.
The usual lifecycle is:

```powershell
sonda autostart start
sonda autostart stop
sonda autostart restart
sonda autostart uninstall
```

If installation used a non-default configuration path, pass that same
`-config PATH` to `status`, `start`, `stop`, `restart`, and `uninstall`. Those
commands derive the expected task locally instead of trusting metadata stored
inside the task.

The managed task sets the configuration path and working directory absolutely,
runs on battery, keeps one instance, has no 72-hour limit, starts when available,
and retries a failed process three times at one-minute intervals. When the
running binary is the exact `apps/sonda/current` target of a verified Scoop
shim, it uses that shim so `scoop update sonda` does not strand the task on an
old version. A binary run from a release ZIP or local build is treated as
portable: put it in its final location before installing autostart. If it is
moved later, restore the original path or remove the old task manually in Task
Scheduler before installing again; Sonda will not overwrite the changed action.

Stopping signals only the Sonda instance identified by this task and lets its
capture queue drain. If graceful signaling fails while the canonical task is
unchanged and its expected control event still proves that the managed process
is active, the sole fallback is to end that exact scheduled task; status reports
that the fallback was abrupt. Sonda never kills every process named `sonda.exe`.

Autostart refuses a configuration whose `api_listen` is not loopback. The
explicit `-allow-non-loopback` override exists for isolated environments, but
it exposes an unauthenticated API that can read captures and change proxy state.
`uninstall` removes only the task; configuration, database, certificate
authority and logs remain in place. Logs go beside the configuration as
`sonda.log`, with one bounded rotated copy.

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

Same as above from here: open **http://127.0.0.1:9000** and send it the three
requests in the Docker block — the ports are the same, because
`sonda.example.yaml` and the compose configuration describe the same two toy
services.

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

The name is the one the address was read from when the project was imported —
`MS_AUTH_ADDR`, `MS_AUTH_HOST`, whatever the file actually says. It is only
derived from the service and its protocol when Sonda has no record of a name,
which is a service added by hand or read from a compose file: a guessed name
served beside the real one is a line that sets a variable nothing reads.

### The configuration file

`sonda.yaml` still carries the process-level settings — where the API listens,
how much of a body to keep, how long captures live. Its `targets` are only a
**seed**: they become the first project the first time a database is created,
and are ignored afterwards, so an edit made in the interface is never undone by
a stale file. Running with no configuration file at all is an ordinary first
run.

## I pointed it at my service and I see nothing

This is the most common first run of a tool like this, and every cause of it
looks identical from outside: an empty screen. So Sonda answers the question
instead of leaving it. **When nothing has been captured, the field stops being
empty and becomes a reading** — one line per channel with what Sonda knows about
it, and the same answer is available in the terminal, over the API and to an
agent:

```bash
curl -s localhost:9000/api/diagnose | jq
```

```
sonda-tui              the inspector shows it while the field is empty
diagnose_silence       the MCP tool an agent calls when a capture is missing
```

### What it can tell you

Each channel gets a verdict, and the numbers behind it are on screen:

| Verdict | What it means |
|---|---|
| `capturing` | Calls are being recorded here. An empty field is the filter, the window or the selected channel — not the proxy |
| `listener_down` | The port never opened, usually because something else holds it. Nothing can arrive here at all, and the error says what |
| `connected_not_captured` | Something reached this port and never became a call. Sonda saw the connection and did not understand what came down it |
| `upstream_unreachable` | The service behind Sonda refused a connection when asked. Only ever reported after an explicit probe |
| `no_connections` | Nothing has touched this port since it opened |

The reading that does the most work is **`connections`**, which counts every TCP
connection the port accepted whether or not it became a call. Connections
without captures is a client that found Sonda and was misunderstood — a client
speaking TLS to a plaintext listener or the reverse, or a protocol Sonda does
not proxy at all. Zero connections is a client that never arrived. Those are
different problems with different fixes, and without that count they read
exactly the same.

Sonda proxies HTTP (including WebSocket upgrades and server-sent events), gRPC,
PostgreSQL and AMQP 0-9-1. A Kafka, Redis or plain TCP client pointed at a Sonda
port is accepted and never understood, which shows up as
`connected_not_captured` rather than as silence.

### What it cannot tell you, and says so

**Sonda cannot see a client that never connected to it.** A port with no
connections reads the same way whether the caller is still talking to the
service directly, is pointed at the wrong port, or has simply not run yet. There
is no honest signal that separates those three, so the report names all of them
and gives the one thing that does separate them: point the caller at Sonda,
trigger the call, and watch the connection count. It moves even when the request
itself is wrong. If it stays at zero, nothing is reaching Sonda.

### Probing an upstream is a side effect

Finding out whether the service behind Sonda is up means dialling it, and that
is traffic the user did not send. So it never happens on its own — not on a page
load, not on a refresh, not on a timer:

```bash
# reads only what Sonda already knows, touches no network
curl -s localhost:9000/api/diagnose

# additionally dials each upstream once and hangs up
curl -s -X POST localhost:9000/api/diagnose
```

The press is the only way to ask for it: `PROBE UPSTREAMS` in the browser, `p`
in the terminal, `probe_upstreams` on the MCP tool. The dial sends no bytes and
goes **straight to the service, never through Sonda's own listener**, so a probe
can never turn up in the capture list looking like a call you made.

### Still nothing

- **Is a project active?** No active project means no open ports, and the report
  says that before anything else.
- **Did the caller reload its configuration?** A process started before the
  environment variable changed still holds the old address.
- **Is the scheme right?** A listener that terminates TLS answers nothing on
  `http://`, and a plaintext one answers nothing on `https://`. The line each
  service hands over carries the scheme for that reason.
- **Check Sonda's own log.** A refused TLS handshake is reported there and
  nowhere else, because it fails before a call exists to attach it to.

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
S O N D A  ■ LIVE   FAULTS  ALL    1M  5M  30M                      19 CAPTURED  ·  2 FLAGGED
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
 ↑↓ chan · ←→ call · g/G ends · ⏎ read · esc close · t tree · c contract · r replay · d diff · f faults · w window · h hold · / find · q quit
```

The translation is mostly direct — monospace is free here, hairlines become
box-drawing characters, and channel colours carry over unchanged. Two things
needed a different expression:

- There are no type sizes, so the four roles become weight and dimming.
- A lane is one row tall, so a fault cannot be a taller bar. It becomes a **full
  block where an ordinary call is a half one** (`█` against `▄`), with a third
  glyph for a cell holding both. Shape still carries the outcome before colour
  does, which is the rule that matters.
- A service being **broken on purpose** is a mode and not a call, so the same
  block is engraved on the channel, before its name, and the bar counts how many
  are armed — the browser's badge and readout in the two places this client has
  for them.

| Key | |
|---|---|
| `↑` `↓` or `k` `j` | pick a channel |
| `←` `→` or `H` `L` | step along it, call by call |
| `home` or `g` | jump to the oldest call on the channel |
| `end` or `G` | jump to the newest |
| `enter` | read the selected call |
| `esc` | close whatever is open: inspector, diff, tree or contract |
| `t` | show the whole request it belonged to, as a tree |
| `c` | has this endpoint changed shape since it used to work |
| `r` | replay it |
| `d` | diff a replay against its original |
| `f` | faults only, or everything |
| `w` | cycle the sweep |
| `h` | hold the trace |
| `/` | search; `enter` applies it, `esc` clears it and leaves the field |
| `q` or `ctrl+c` | quit |

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

# and the contrast: only the ones that did not. Leave failed out for both
curl 'http://127.0.0.1:9000/api/calls?failed=false'
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
cookies, API keys, personal data. Application payloads are not redacted on the
way in, and that is a deliberate choice rather than an oversight — redacting a
replayable capture would mean it is no longer what was sent.

Two connection-handshake exceptions make the safer trade-off: a
**Postgres** password and **AMQP SASL challenges and responses** are blanked in
the stored copy as the bytes go past. Neither capture can be replayed without
the connection state that is already gone, and the alternative is a live
credential in a plaintext file. See [PostgreSQL](#postgresql) and
[AMQP 0-9-1](#amqp-0-9-1).

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
| `search_calls` | By service, method, path, status, or text in the bodies. `failed` takes three states: true for the failures, false for what worked, absent for both |
| `get_call` | One call in full, decoded |
| `diff_calls` | "This one worked and this one did not — what changed?" |
| `trace_call` | Every call that was part of the same request, as a tree |
| `list_services` | What is being observed, on which ports, whether it is listening — and what is stubbed or being broken right now |
| `schema_status` | Where each gRPC service's field names came from: reflection, the descriptor set, or nothing |
| `wait_for_call` | Blocks until matching traffic appears. Trigger something, then verify it. `failed` takes the same three states |
| `replay_call` | Send a capture again. Marked destructive, so clients ask first |
| `connect_project` | Set Sonda up to watch a whole system, and hand back the edit that makes traffic flow through it. Safe to run again |
| `configure_service` | Add one service, or change one that is already there — the name is the identity, so calling it again moves the port. An update keeps every setting it was not asked about |
| `remove_service` | Delete one service, and say what address to point the caller back at. Asks first |
| `upload_schemas` | Give a project a compiled descriptor set, so gRPC decodes where no service serves reflection. Local stdio accepts a file `path`; HTTP MCP accepts base64 content |
| `activate_project` | Open the ports. Asks first |
| `disconnect_project` | Close them and hand back the edit that undoes the pointing. Asks first |
| `set_stub` | Answer for a service from recordings instead of forwarding. Asks first |
| `break_service` | Add latency, force a status, or cut the connection. Asks first |
| `contract_drift` | Has this response changed shape since it used to work |
| `trust_certificate` | The certificate authority's own bytes, where Sonda keeps it, and what to run to trust it or take it back out |
| `diagnose_silence` | "Why am I seeing nothing?" — per service: whether the port opened, whether anything connected, what was captured, and which causes cannot be told apart |

`wait_for_call` is the one that turns Sonda into a check rather than a viewer:
the agent makes a change, triggers the action, and waits for what should have
gone over the wire. Nothing arriving is also an answer.

`upload_schemas` has two transports with deliberately different file access:

```json
{ "project": "core-delpagroup", "path": "C:\\work\\descriptors.binpb" }
```

That `path` form is available only to the local `sonda mcp` stdio process. It
accepts a file up to 32 MiB from the machine that launched it, keeps the path's
base name as the descriptor name, and sends the bytes to Sonda. The HTTP MCP
endpoint at `/mcp` never reads a path from Sonda's filesystem. Use the existing content
form there, or over stdio when the JSON stays within its message limit:

```json
{ "project": "core-delpagroup", "filename": "descriptors.binpb", "content_base64": "..." }
```

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

The inverse only ever names a variable Sonda actually saw. `MS_AUTH_ADDR`,
`MS_AUTH_HOST` and `MS_AUTH_HTTP_URL` are all accepted on the way in, so
rebuilding the name out of the service and its protocol would hand back
`MS_AUTH_URL` — a variable nothing reads, while the real one still points at a
port that just closed. The name it did see is kept with the service, so
connecting in the morning and disconnecting in the evening works across a
restart of Sonda or of the machine. Where the name is not known — a service
added by hand, or one read out of a compose file, which never had a variable to
begin with — it comes back under `restore_by_hand`, with the address to search
for and the address to put back:

```json
{
  "changes": { "MS_AUTH_ADDR": { "from": "127.0.0.1:9152", "to": "localhost:50052" } },
  "restore_by_hand": [
    {
      "service": "web",
      "was_listening_on": "127.0.0.1:9100",
      "point_back_at": "localhost:3000",
      "problem": "Sonda does not know which variable pointed at it…"
    }
  ]
}
```

Creating configuration disturbs nobody, so those tools run freely. Opening and
closing ports can pull the floor out from under you mid-debug, so those ask
first.

### Asking twice

Editing the file and asking again is the ordinary next step, not a mistake, so
`connect_project` takes the same name a second time: the project that is already
there is added to, a service it already has is updated in place with whatever
the file says today, and anything the file cannot express — TLS, whether the
upstream's certificate is checked — is kept. A run where nothing could be saved
deletes the project it created on the way in, so a failed attempt leaves nothing
to clean up.

`configure_service` works the same way round: an update starts from the stored
service and changes only what you passed, so moving a port is the project, the
name and the new address. It answers with the address to point the caller at,
and with the variable to write it into when Sonda knows which one that is.
That address is directly usable. The project API's `point_at` field and
`configure_service`'s `listening_on` return `http://host:port` for HTTP
(`https://` with listener TLS), while AMQP returns `amqp://host:port` or
`amqps://host:port`. Plaintext gRPC remains `host:port` (listener TLS makes it
`https://host:port`), and Postgres remains `host:port` for insertion into the
caller's own DSN.

### What is not on MCP, on purpose

- **Deleting a project.** `remove_service` covers a service that has to go, and
  connecting the same project again is how a configuration that changed gets
  applied, so nothing is stuck behind the gap. Throwing away a whole project —
  its services, its schemas, whatever else is in it — is a decision with a
  person's hand on it, and the web interface has the button.
- **The live stream.** `wait_for_call` answers the same question with a bound on
  it, and a server-sent stream held open across a tool call buys nothing an
  agent can use.
- **Installing the certificate authority.** `trust_certificate` hands over the
  certificate and the exact commands — the public certificate is not a secret,
  and an answer the reader cannot act on is not an answer. Running one of those
  commands changes a machine's trust store, and that act stays the user's.
- **Turning redaction off.** There is no such setting anywhere, and MCP is the
  last surface that would get one.

### Credentials do not leave

Everything above is filtered before it goes out, with two gaps named at the end
of this section. `Authorization`, `Cookie`,
`X-Api-Key`, `password`, `client_secret` and their various spellings come back
as `[redacted by Sonda]` — in headers, in bodies, and inside JSON nested in a
body. **There is no setting to turn this off**, deliberately: a flag for it
would be switched on against a toy project and then forgotten against a real
one. The web interface shows the stored application payloads because there the
reader is you; Postgres and AMQP handshake secrets are already absent from every
surface because they were blanked before persistence.

Matching a field name only works on a field that has one, so four more passes
reach where it cannot. Each of them runs at one known place in the answer and
is unreachable from anywhere else — the endpoint a tool called is what says
which fields are Sonda's own, so a captured body that happens to hold a `sql`,
a `detail` or a `postgres` key is left exactly as it was recorded:

- **Query strings**, wherever a URL turns up — the captured path, a `Location`
  redirect, a link inside a body. `?access_token=`, `?code=` and
  `?X-Amz-Signature=` are blanked and the rest of the URL is kept, because the
  path is how you recognise the call.
- **Postgres**, which is column oriented, so the sensitive name and the
  sensitive value arrive in different messages. A `RowDescription` is aligned
  against the `DataRow`s after it, and a statement that names a credential comes
  back with its structure intact and its literals blanked — including in the
  one-line summary a listing shows before you have asked for anything, and in
  the two places a trace repeats that line. The tree drawn as text is not
  scanned for that line: each node reports what its own reading became and the
  exact strings are substituted in, so every node is covered at every depth.
- **AMQP authentication**, whose challenge and response are opaque byte strings
  rather than named fields. Sonda blanks the SASL response in
  `connection.start-ok` and both sides of `connection.secure` before
  persistence. It stores no raw bytes from an incomplete frame that could carry
  a credential. The selected mechanism name, such as `PLAIN`, remains visible.
  Forwarding still uses the original bytes.
- **A changed credential in a diff.** `diff_calls` addresses a changed field by
  a path, so the name is a value and the keys around it are `path`, `a` and `b`.
  When the path names a credential, both sides of the comparison are blanked.
- **The second copy of a decoded capture.** A Postgres session, a WebSocket, an
  event stream, a gRPC call and an AMQP unit are each served twice — decoded, and byte for
  byte as they crossed — and redacting the first copy is worth nothing while
  the second is sitting beside it. The verbatim copy is dropped wherever the
  decoded view replaces it, side by side. Where nothing decodes it, it stays: an
  event stream's request, a compressed gRPC frame, and any view that came back
  empty — a 502 HTML page served as `text/event-stream` is still the only record
  of what happened, and dropping it would leave you with nothing rather than
  with less.

Two gaps, both deliberate. A protobuf field decoded **without** a schema has a
number and no name, so there is nothing for name matching to match and the value
comes back in the clear; give the project a descriptor set, or the service
reflection, and the field has its name back and is redacted like anything else —
`schema_status` says which of the two you are getting. And a service's own error
message — a transport error, a gRPC status — is
returned as written: reading prose as SQL cuts it at the first apostrophe, and
blanking any line that names a credential loses `Internal: couldn't refresh the
session cookie` in the tool that exists to show failures.

Bodies are also shortened by default; `get_call` takes `detail` for the whole
thing. `detail` does not reveal credentials — redaction runs over the whole
payload first and shortening second, so the default answer is never the leakier
one. Both are covered by tests, one of which goes through a real tool call.

`SECURITY.md` lists what redaction reaches and, more usefully, what it does not.

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
plaintext and could reach an agent through MCP — whose redaction reads field
names, query strings and the shape of a Postgres exchange, and a startup
handshake is none of those: the password is a length-prefixed run of bytes in a
TCP stream, under no name at all.

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

## AMQP 0-9-1

Declare RabbitMQ as an `amqp` target and point the client at Sonda, keeping the
client's own user, password and virtual host:

```yaml
  - name: events
    listen: 127.0.0.1:9401
    upstream: amqp://127.0.0.1:5672
    protocol: amqp
```

```text
AMQP_URL=amqp://app:change-me@127.0.0.1:9401/orders
```

The configured upstream must not contain credentials. Sonda forwards the
client's own handshake byte for byte and has no use for a second copy of the
password in configuration.

Use `amqps://` when the broker hop speaks TLS. Listener TLS is independent:

```yaml
  - name: secure-events
    listen: 127.0.0.1:9402
    upstream: amqps://rabbitmq.internal:5671
    protocol: amqp
    tls: true
```

Here Sonda verifies the broker certificate and the caller uses
`amqps://127.0.0.1:9402`. As with HTTPS, the caller must trust Sonda's local
authority. `insecure_skip_verify: true` is accepted only for this one
`amqps://` target and is recorded everywhere; it is not a global switch.

### What one capture means

AMQP is a bidirectional session rather than a request/response protocol, so
Sonda stores useful, direction-specific units instead of inventing pairs:

- `basic.publish`, `basic.return`, `basic.deliver` and `basic.get-ok` include
  their content header and body frames for that channel.
- Handshake, topology, acknowledgement and close methods stand alone. Heartbeat
  frames are forwarded but omitted from storage.
- The list carries the AMQP method and a searchable route, queue, virtual host
  or channel. Message text and broker replies are also indexed.
- Broker `close` and `return` errors with reply codes of 300 or greater are
  faults in the API, web field, terminal and `recent_failures`.

`GET /api/calls/{id}` exposes the decoded frames under `amqp.sent` and
`amqp.received`, plus `sent_incomplete` and `received_incomplete`. The web
inspector and TUI render those same frames. MCP `get_call` returns that decoded
view and removes the redundant raw copy; `search_calls` accepts
`protocol: "amqp"` and the same method, path and text filters as other captures.

### Credentials and limits

What the broker receives is untouched. In the copy stored by Sonda, SASL
challenges and responses from `connection.start-ok`, `connection.secure` and
`connection.secure-ok` are zeroed while the selected mechanism name remains.
If a connection ends in the middle of a frame that could contain a credential,
Sonda records its size and error but stores none of those partial raw bytes.

A frame larger than the 16 MiB capture parser limit is still forwarded, but is
stored only as size and error metadata. `max_body_bytes` independently limits
how many bytes of an otherwise valid unit reach SQLite; forwarding is still
complete. These captures cannot be replayed, stubbed or fault-injected: doing so
would require recreating a connection and channel state that no longer exists.
Sonda supports AMQP **0-9-1**, not AMQP 1.0, and does not reconstruct broker
transactions or pair publishes with deliveries.

## TLS

Two different problems wear the same three letters, and Sonda answers them
separately.

### The upstream speaks TLS

Declare it with the scheme it has and nothing else changes:

```yaml
- name: payments
  listen: 127.0.0.1:9103
  upstream: https://api.payments.example.com
  protocol: http
```

The certificate is verified the way any other client would verify it. A gRPC
target behind TLS works the same way — it negotiates HTTP/2 over ALPN instead of
h2c — and an upstream written without a port gets the one its scheme implies.

An upstream whose certificate does not verify answers 502 with the reason, and
the capture records the transport error. That is deliberate: a proxy that
quietly accepted any certificate would make every "verified" reading in the tool
worthless.

### The client speaks TLS

Some clients refuse `http://` outright. `tls: true` makes Sonda answer that port
with a certificate of its own:

```yaml
- name: web-api
  listen: 127.0.0.1:9104
  upstream: http://127.0.0.1:3000
  protocol: http
  tls: true
```

The caller is then pointed at `https://127.0.0.1:9104`, and the interface hands
over that exact line. The certificate is minted on the fly for whatever name the
client asked for — the SNI name, or the address it connected to when it sent
none — cached in memory per name, and signed by an authority Sonda generates the
first time one is needed.

Postgres is the exception. A database negotiates encryption inside its own
protocol rather than before it, so a TLS listener in front of it would be
answering a handshake no client sends. Sonda refuses the flag there rather than
accepting it and ignoring it.

### Trusting the certificate authority

**Sonda does not install anything.** It writes two files beside the database —
`sonda-ca.pem` and `sonda-ca-key.pem`, the key owner-only — prints what to run,
and stops. Modifying your machine's trust store is your decision to make
deliberately; a debugging tool that did it quietly would be indistinguishable
from malware.

The commands are printed the first time the authority is actually needed — when
a service with `tls: true` first starts, which is not the same as the first run:
a Sonda with no TLS target never creates one and prints nothing. They are also
shown in the interface's certificate authority panel, and returned by the
`trust_certificate` MCP tool. The narrow option is usually the right one,
because it trusts nothing else on the machine and leaves nothing to undo:

```bash
curl --cacert ./sonda-ca.pem https://127.0.0.1:9104/
NODE_EXTRA_CA_CERTS=./sonda-ca.pem npm start
SSL_CERT_FILE=./sonda-ca.pem go run ./cmd/whatever
REQUESTS_CA_BUNDLE=./sonda-ca.pem python app.py
```

Every one of those lines names a path, and when Sonda runs in a container the
path it prints is a path inside the container — there is no `/data` on your
machine. Get the file onto your own disk first, then use that path instead:

```bash
docker compose cp sonda:/data/sonda-ca.pem ./sonda-ca.pem
# or, without Docker in the way
curl -o sonda-ca.pem http://127.0.0.1:9000/api/tls/ca.pem
```

An agent has neither: `trust_certificate` returns the certificate itself in
`certificate_pem`, so it can write the file and hand over the path it wrote.

For the whole machine — which is what a browser needs — run the line for your
platform yourself:

```bash
# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ./sonda-ca.pem
# Windows
certutil -user -addstore Root .\sonda-ca.pem
# Linux (Debian, Ubuntu)
sudo cp ./sonda-ca.pem /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates
# Linux (Fedora, RHEL)
sudo cp ./sonda-ca.pem /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust
```

Firefox keeps its own store: **Settings → Privacy & Security → Certificates →
View Certificates → Authorities → Import**, then tick *Trust this CA to identify
websites*.

And to take it back out again — withdraw the trust first, delete the files
second, or the machine keeps trusting a root nobody can account for:

```bash
# macOS
sudo security delete-certificate -c "Sonda local CA (your-hostname)" /Library/Keychains/System.keychain
# Windows — the serial is printed with the certificate and shown in the interface
certutil -user -delstore Root <serial>
# Linux (Debian, Ubuntu)
sudo rm /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates --fresh
# Linux (Fedora, RHEL)
sudo rm /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust

rm ./sonda-ca.pem ./sonda-ca-key.pem
```

The authority names itself and the machine it was made on — `Sonda local CA
(hostname)` — so it is findable in a trust store a year later, and it expires
after a year, so one that is forgotten stops mattering on its own. The private
key is never logged, never returned by the API, never reachable over MCP and
never written into a capture; `SECURITY.md` says what that means for copying the
database around.

### Not verifying an upstream

The developer case this exists for is a service with a self-signed certificate.
There is exactly one way to skip the check, and it is per service:

```yaml
- name: staging
  listen: 127.0.0.1:9105
  upstream: https://staging.internal:8443
  protocol: http
  insecure_skip_verify: true
```

There is no process-wide switch and there is not going to be: "I trust this one
container" and "I trust anything" are not the same statement. Sonda refuses the
flag on a plaintext upstream, where it would mean nothing while still reading as
unverified everywhere.

And it is never quiet. The service is marked in the web interface, in the
terminal's channel rail and in `list_services`, and every capture taken through
it carries `upstream_insecure` — so a response read months later still says
whether anyone ever checked who sent it.

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

In the interface it is on the service itself, under **PROJECTS**: the row
carries `LATENCY MS`, `HTTP STATUS`, `CUT` and `ONE CALL IN` beside an `ARM`
key, and reads back **BROKEN ON PURPOSE** with the rule in force until
`RESTORE` takes it off. A rule that would do nothing — no latency, no status,
no cut — is refused, and what the panel shows is the refusal rather than a rule
that was never armed.

The terminal reads the same state and does not set it, which is the level it
already works at for stubbing: the bar counts what is armed, the channel
carries the fault block before its name, and the inspector names the rule
beside the call being read.

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

A rule in force is stated where the failures are being read, not only where it
was armed: the channel shows **BROKEN**, and the readout at the top of the
browser and the bar in the terminal both count what is armed. That matters most
above `one_in: 1`, where most calls pass through untouched and the injected ones
on their own look like a service that is merely flaky.

Rules are forgotten when Sonda restarts, for the same reason stubbing is: a
service that has been failing since Tuesday because of a rule nobody remembers
setting is a worse afternoon than the bug being chased. Nothing about them is
written to the database.

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
    protocol: http     # http, grpc, postgres or amqp

  - name: payments
    listen: 127.0.0.1:9103
    upstream: https://api.payments.example.com  # verified like any other client
    protocol: http
    tls: true                    # answer this port with a certificate, for callers that refuse http://
    insecure_skip_verify: false  # per service, never global. See TLS below

  - name: events
    listen: 127.0.0.1:9401
    upstream: amqp://127.0.0.1:5672  # amqps:// for a TLS broker
    protocol: amqp
```

Then point whatever calls `admin-api` at `127.0.0.1:9102`. The same binary and
the same file work for services in containers and for services running natively
— which is the point, since a real local stack is usually both.

Inside Docker, use `host.docker.internal` to reach a service running on the
host. See `sonda.docker.yaml`.

## API

| Method and path | Purpose |
|---|---|
| `GET /api/calls` | List captures, newest first. Filters: `target`, `method`, `path`, `status`, `protocol`, `grpc_status`, `failed`, `q`, `since`, `until`, `limit`, `before_id`. `failed=true` is only the failures, `failed=false` only the calls that did not fail, and leaving it out is both. |
| `GET /api/calls/{id}` | One capture with headers and bodies, plus the protocol-specific decoded view (`grpc`, `socket`, `stream`, `graphql`, `postgres` or `amqp`) when applicable. |
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
| `GET /api/runtime` | Which project is active and what is really listening, including how many connections each port has accepted. |
| `GET /api/diagnose` | Why nothing is being captured, per service. Reads only what Sonda already knows and touches no network. |
| `POST /api/diagnose` | The same report, plus one TCP dial to each upstream. A side effect, which is why it is not on the `GET`. |
| `GET /api/tls` | The certificate authority: the certificate itself in `certificate_pem`, the exact commands to trust it and to remove it, and — when Sonda is in a container — the `docker cp` that copies the file out. Never the private key. |
| `GET /api/tls/ca.pem` | Download the CA certificate. Useful when Sonda runs in a container. |
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
