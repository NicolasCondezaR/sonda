[← Docs](README.md)

# Agents

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
| `diff_flows` | "This worked yesterday and today it does not" — two whole runs aligned, and the first call where they parted ways |
| `trace_call` | Every call that was part of the same request, as a tree |
| `list_services` | What is being observed, on which ports, whether it is listening — and what is stubbed or being broken right now |
| `schema_status` | Where each gRPC service's field names came from: reflection, the descriptor set, or nothing. When a service resolved nothing it also names the services affected and the command that compiles a descriptor set for them |
| `wait_for_call` | Blocks until matching traffic appears. Trigger something, then verify it. `failed` takes the same three states |
| `arm_trigger` | "Tell me when this happens again" — arms a condition that outlives the session, unlike wait_for_call which blocks for at most two minutes. Read what it caught with `list_services` |
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

### What an answer costs an agent

Everything an agent reads comes out of its context, so an answer that repeats
itself is a real cost with nothing bought. Two bounds exist for that, and both
apply to MCP only — the web and the terminal render every service and every
frame at no cost to anyone, and a capability that behaves differently per client
is one nobody can reason about.

- **Readings that are the same are stated once.** `diagnose_silence` on a project
  of twenty-two quiet services used to return twenty-two copies of one paragraph,
  differing only in the address spliced into each sentence: about 6.900 tokens,
  96% of it per-service entries. It now groups services whose reading is
  identical, states the shared sentences once with `{listen}`, `{point_at}`,
  `{expects}` and `{upstream}` standing for each member's own field, and lifts
  the facts every member agreed on into `same_for_all`. The same report is about
  2.100 tokens. **Nothing is dropped**: every original sentence can be
  reconstructed from the placeholders and the fields beside them, and a reading
  that genuinely differs — two services capturing different numbers of calls —
  stays separate rather than being folded into its neighbour.
- **Long strings and long lists say what they left out.** A string over 2.000
  characters is cut, and a list over 24 entries keeps both ends with a marker in
  the middle naming how many are missing. Both ends, not the first 24: a stream's
  outcome is at its end, so keeping only the head would drop the part being
  debugged. `detail: true` on `get_call` returns everything, whole.

Neither bound is a summary. A summary decides for the reader which services
matter, and the reader is the one debugging.

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

