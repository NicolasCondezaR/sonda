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
| `internal/api` | The HTTP API every client reads, including the decoded views |
| `internal/mcp` | The tools an agent calls, over HTTP or a pipe |
| `internal/web` | The interface, embedded in the binary. Plain HTML, CSS and JavaScript |
| `internal/tui` | The terminal client, a second consumer of the same API |
| `internal/grpcwire`, `wsframe`, `protoschema`, `trace`, `drift`, `calldiff` | Pure decoding and comparison, tested without a network |

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
