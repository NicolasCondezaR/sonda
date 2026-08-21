[← Docs](README.md)

# I pointed it at my service and I see nothing

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

