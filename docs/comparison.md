[← Docs](README.md)

# Related work

Sonda is not the first tool to capture gRPC. Anyone evaluating it deserves the
map before the pitch, including the cases where something else is the better
answer.

| Tool | What it is | Where it overlaps | Where it stops |
|---|---|---|---|
| [`bradleyjkemp/grpc-tools`](https://github.com/bradleyjkemp/grpc-tools) | `grpc-dump`, `grpc-replay`, `grpc-fixture` — Go CLIs | The closest of all: capture, replay and canned responses, the same three ideas | gRPC only, output is a JSON stream on disk, no query API, no interface, no other protocol beside it |
| [`Rantanen/proxide`](https://github.com/Rantanen/proxide) | HTTP/2 and gRPC debugging proxy with a Rust TUI | Captures to a file, decodes with descriptors | No API, so nothing else can read the capture; one protocol |
| [Fiddler Everywhere](https://www.telerik.com/fiddler/fiddler-everywhere/documentation/capture-traffic/advanced-capturing-options/capturing-grpc-traffic) | Commercial GUI debugger | Decodes gRPC through reflection or `.proto` files | Commercial, GUI-first, HTTP/2 capture is opt-in; built for a human reading one call |
| [HTTP Debugger](https://www.httpdebugger.com/debug/grpc-calls) | Commercial, Windows | Reads HTTP/2, protobuf framing and `grpc-status` trailers off the wire, without a proxy | Windows only, commercial, one machine |
| [Wireshark + gRPC/protobuf dissectors](https://grpc.io/blog/wireshark/) | Network protocol analyser | Sees the frames, with schemas if you feed it some | It is an analyser: no replay, no stub, no fault injection, no correlation between a call and the calls it caused |
| [Kubeshark](https://github.com/kubeshark/kubeshark) | eBPF network observability for Kubernetes | Multi-protocol — HTTP, gRPC, Kafka, Redis, AMQP, DNS — and it exposes MCP to agents too | It is Kubernetes. It wants a cluster, a DaemonSet and eBPF; it is not the thing you run against three services on a laptop |

## What Sonda does that none of them do together

- **One capture across protocols.** HTTP, gRPC, WebSocket, server-sent events,
  GraphQL operations, PostgreSQL statements and AMQP units land in the same
  store, searchable side by side, because a bug between services rarely stays
  inside one protocol.
- **It decodes protobuf with no schema at all.** Reflection first, then the
  project's descriptor set, and when neither exists — the normal case in a
  monorepo whose services serve no reflection — it decodes the wire format
  structurally rather than giving up.
- **The API is the product, and the interfaces are its clients.** The web
  interface, the terminal client and the [MCP server](agents.md) read the same
  HTTP API. A coding agent is a first-class consumer, not an export format.
- **Capture is the floor, not the ceiling.** [Replay and structural
  diff](replay.md), [stub mode, fault injection and contract
  drift](experiments.md) all work on the bytes that were actually recorded.
- **One static binary and one SQLite file.** No cluster, no eBPF, no C
  toolchain, no daemon to keep alive. `brew install`, or `docker compose up`.

## When to use something else

- **You only need gRPC and only need a dump.** `grpc-tools` is smaller and does
  that well.
- **Your traffic is in a Kubernetes cluster.** Kubeshark is built for that and
  Sonda is not: Sonda is an explicit per-port proxy for local development.
- **You want packet-level truth about a network you do not control.** That is
  Wireshark, and it always will be.
- **You want a polished commercial GUI with support behind it.** Fiddler.

Sonda is aimed at the case that was left over: several services on one machine,
mixed protocols, no schemas to hand, and a coding agent that should be able to
read what crossed the wire without a human pasting logs into a prompt.
