# Sonda

A capturing proxy for local development traffic. Point a client at Sonda
instead of the service it talks to, and the HTTP calls, gRPC streams, database
statements and AMQP units that cross it become searchable, comparable where that
is meaningful, and readable by a coding agent as well as by you.

[![CI](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml/badge.svg)](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

*[Léeme en español](README.es.md)* · **[Documentation](docs/README.md)**

![The event field: one lane per service, faults as full-height bars](docs/assets/sonda-field.jpg)

## Why

Debugging between services usually means reading logs from several containers,
none of which contain the payload. `mitmproxy` solves that for HTTP. For gRPC
the ground is thinner but not empty — `grpc-tools` and `proxide` capture it,
Fiddler decodes it, Wireshark dissects it — and what none of them do is hold
several protocols in one capture, decode protobuf when no schema is available at
all, and hand the result to a coding agent through an API. That is the gap Sonda
is aimed at. The honest map, including when to use one of those instead, is in
[Related work](docs/comparison.md).

## How it works

An explicit proxy: one listening port per observed service. Nothing is
intercepted behind your back, and a service that is not configured is not
captured.

```
client ──▶ sonda :9101 ──▶ your service :3000
                │
                └──▶ SQLite ──▶ query API :9000
```

Two properties drive the design. **Forwarding is byte exact** — a debugger that
alters the traffic invalidates every conclusion drawn from it, with [one named
exception](docs/replay.md#where-a-trace-id-comes-from): a request with no trace
id of its own gets one written before it is forwarded, so what it causes can
still be grouped. And **the stored bytes are the record**: bodies are saved
exactly as they crossed the wire and decoded only when displayed, so replay
stays meaningful and an old capture becomes readable once its schema shows up.
The two storage exceptions, PostgreSQL passwords and AMQP SASL exchanges, are
blanked before anything is written. See
[Storage, behaviour and cost](docs/storage.md).

## What it captures

| | |
|---|---|
| **HTTP/1.1 and HTTP/2** | Requests, responses, headers, bodies |
| **gRPC** | Unary and streaming, trailers preserved, protobuf decoded — by reflection, by descriptor set, or [structurally from the wire format when neither exists](docs/protocols.md#grpc) |
| **WebSocket and server-sent events** | Frame by frame, [handshake relayed verbatim](docs/protocols.md#sockets-and-event-streams) |
| **GraphQL** | The [operation read out of the body](docs/protocols.md#graphql), so identical POSTs stop being indistinguishable |
| **PostgreSQL** | [The wire protocol](docs/protocols.md#postgresql): statements, parameters, results |
| **AMQP 0-9-1 and AMQPS** | [Publishes, deliveries and acknowledgements](docs/protocols.md#amqp-0-9-1) as units of work |
| **TLS** | [A local CA that installs nothing](docs/protocols.md#tls), for the services that only speak HTTPS |

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

Everything binds to `127.0.0.1` on purpose: the capture holds whatever crossed
the wire. Full detail, Linux notes and the PowerShell quirks are in
[Installing Sonda](docs/install.md).

## Quick start

```bash
docker compose up -d
```

That starts Sonda plus two toy upstreams to have something to capture: an `echo`
HTTP service and a `grpcdemo` gRPC service. Their own ports are deliberately not
published — traffic is meant to arrive through the proxy.

Open **http://127.0.0.1:9000** and send it something:

```bash
curl http://127.0.0.1:9101/ok                 # HTTP, through Sonda
curl 'http://127.0.0.1:9101/fail?status=503'  # a fault, to see one flagged
grpcurl -plaintext -d '{"order_id":"ORD-1"}' \
  127.0.0.1:9201 demo.v1.Orders/GetOrder      # gRPC, through Sonda
```

Without Docker, the same three requests reach the same ports:

```bash
go build -o sonda ./cmd/sonda
go build -o echo ./examples/echo
go build -o grpcdemo ./examples/grpcdemo

cp sonda.example.yaml sonda.yaml
./echo -addr 127.0.0.1:8081 &
./grpcdemo -addr 127.0.0.1:8082 &
./sonda -config sonda.yaml
```

Pointing it at your own services is a matter of naming them once — Sonda can
read a project's `.env` or compose file rather than have fifteen services typed
in by hand. See [Projects and configuration](docs/configuration.md), and
[It captures nothing, now what](docs/troubleshooting.md) if a port stays quiet.

## For coding agents

Sonda ships an MCP server, so an agent debugging your code can ask what actually
crossed the wire instead of asking you to paste logs:

```
recent_failures      what just broke
diff_calls           this one worked and this one did not — what changed
diff_flows           two whole runs aligned, and where they parted ways
arm_trigger          catch the next call that crosses a condition, hours from now
wait_for_call        trigger something, then verify what went over the wire
trace_call           the tree of calls one request caused
```

Credentials never come back: authorization headers, cookies and similar fields
are redacted before the answer leaves the process, and that cannot be turned
off. The web interface, the terminal client and the MCP server are all clients
of the same [HTTP API](docs/api.md). See [Coding agents](docs/agents.md).

## Beyond capture

Capture is the floor. On top of the recorded bytes:

- **[Replay and diff](docs/replay.md)** — send a stored call again, or compare
  two structurally to see what actually differs.
- **[Compare two runs](docs/replay.md#comparing-two-runs)** — align a flow that
  worked with one that did not, and name the first call where they parted ways.
  Ids in the paths do not stop two runs matching.
- **[Stub mode](docs/experiments.md#stub-mode)** — answer for a service from its
  own recordings instead of calling it.
- **[Breaking things on purpose](docs/experiments.md#breaking-things-on-purpose)**
  — latency, forced statuses, cut connections.
- **[Contract drift](docs/experiments.md#contract-drift)** — what a service
  started sending that its schema never promised.
- **[The trigger](docs/experiments.md#the-trigger)** — arm a condition and walk
  away; come back to the moment it fired. It never matches backwards, and it
  never takes the view from someone already reading it.

## Status

Phase 20 complete: capture, decoding, storage, search, the query API, the web
interface, replay, structural diff, a terminal client, project management,
request trees, stub mode, fault injection, contract drift, the MCP server, TLS,
AMQP 0-9-1, flow diff and the trigger all work, and the whole thing runs from `docker compose up`. See
the [Roadmap](docs/roadmap.md).

## Contributing

Tests use the real thing — real SQLite files, real HTTP servers, a real gRPC
client and server. See [CONTRIBUTING.md](CONTRIBUTING.md), and
[SECURITY.md](SECURITY.md) for anything that should not be reported in public.

MIT. See [LICENSE](LICENSE).
