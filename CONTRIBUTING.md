# Contributing

## Before anything

```bash
go test ./...
go vet ./...
gofmt -l .
```

`go test -race` needs cgo, and this project deliberately needs no C toolchain —
the SQLite driver is pure Go, which is also why the binaries are static. The
race detector therefore runs in CI, or locally in a container:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.26 go test -race ./...
```

It is not a formality. The proxy reads a request body on the transport's
goroutine while the handler reads the capture, and the fault and stub registries
are written from an HTTP handler while the proxy reads them on every call.

## Benchmarks

```bash
go test ./internal/proxy/ -bench=. -benchmem -run=XXX
```

They measure what inserting Sonda costs, and **only the difference between the
paired cases means anything**. An absolute figure is mostly loopback and two
`httptest` servers; the number worth reading is Proxied minus Direct, and then
Proxied minus the bare `httputil.ReverseProxy`, which separates what capture
costs from what being a proxy costs. `-count=5` and a couple of thousand
iterations, or the noise is larger than the thing being measured.

**A loaded machine produces nonsense, and the pairs are how you catch it.**
While these were being written, a run on a busy laptop reported the bare reverse
proxy as slower than Sonda — which cannot be true, since Sonda is that proxy
plus work. That is not a finding, it is the signal to close things and run it
again. `BenchmarkHTTPSmallProxiedDelayed2ms` is the other check: it injects a
2 ms fault, so if the proxied-minus-direct difference does not grow by roughly
2 ms, the harness is measuring something other than time spent inside Sonda and
none of the rest can be trusted.

The `drops/op` metric on the proxied cases says how many captures the buffer
threw away instead of writing. A run that drops most of them is not measuring
the storage path.

## What the code expects of a change

**Tests use the real thing.** Real SQLite files, real HTTP servers, a real gRPC
client and server, a WebSocket handshake computed by hand. There are no mocks of
any of them, and a test that mocks the protocol it is meant to prove does not
prove it.

**A capability is not finished until it exists on every surface.** Sonda has
five: the proxy, the HTTP API, the MCP tools, the web interface and the terminal
client. A feature that only an agent can see, or only the browser, is half a
feature — that mistake has been made here twice and cost a follow-up PR each
time.

**Comments say why, not what.** The code is readable; the reasoning behind a
decision is not recoverable from it. Explain the trade-off, the failure the line
prevents, or the obvious alternative that was rejected.

**Do not weaken the two rules the product is built on.** Forwarding is byte
exact — nothing may imply Sonda changed what crossed the wire. And guesses are
labelled: an inferred field type, a tree grouped by timing rather than by a
trace id, a stubbed answer. Confident nonsense is worse than an honest gap.

## Layout

| | |
|---|---|
| `internal/proxy` | Forwards and captures. The gRPC, WebSocket, stub and fault paths live here |
| `internal/store` | SQLite: captures, projects, services, search, retention |
| `internal/config` | The configuration file: parsing, defaults, and the validation that turns a typo into a startup error |
| `internal/runtime` | Turns stored services into open ports. Every change to configuration ends in its one Reconcile |
| `internal/supervisor` | Owns the listeners, so a port that will not open fails alone instead of taking the others with it |
| `internal/tlsca` | The local certificate authority and the certificates issued from it. It never installs anything |
| `internal/api` | The HTTP API every client reads, including the decoded views |
| `internal/mcp` | The tools an agent calls, over HTTP or a pipe |
| `internal/web` | The interface, embedded in the binary. Plain HTML, CSS and JavaScript |
| `internal/tui` | The terminal client, a second consumer of the same API |
| `internal/discover` | Reads a project's own `.env` or compose file so fifteen services need not be typed in by hand |
| `internal/stub` | Answering for a service from a recording instead of forwarding to it |
| `internal/fault` | Breaking a service on purpose: latency, forced statuses, cut connections |
| `internal/graphql` | Reads the operation out of a GraphQL body, so identical POSTs stop being indistinguishable |
| `internal/grpcwire`, `wsframe`, `pgwire`, `amqpwire` | The wire formats, decoded on the way out and never re-serialized. Built to the same shape on purpose: a new protocol should look like these |
| `internal/protoschema`, `trace`, `drift`, `calldiff` | Schema resolution and comparison, tested without a network |

`PRODUCT.md` holds the product record and `DESIGN.md` the visual system. A
change to the interface is expected to hold to `DESIGN.md`, or to argue with it
on purpose.

## Protos

The example service's protobuf code is generated. Never edit a `.pb.go` by hand:
it carries its own descriptor embedded as raw bytes, and changing text in it
corrupts the length prefixes so the package panics in `init()`. Regenerate:

```bash
buf generate
buf build -o examples/grpcdemo/descriptors.binpb
```

CI checks that rebuilding the descriptor set changes nothing.

## Commits and pull requests

Conventional commits (`feat:`, `fix:`, `docs:`). The body is for the reasoning —
what was decided and why, and what was found along the way. No trailers.
