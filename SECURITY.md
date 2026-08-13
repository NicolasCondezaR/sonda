# Security

## What Sonda holds

Sonda stores the bytes that crossed the wire, and in a real system that means
**bearer tokens, session cookies, API keys and personal data**, in a SQLite file
with no encryption. Captured application payloads are not redacted on the way
in, and that is deliberate: a capture that was altered is no longer what was
sent, which would break the fidelity the tool is built on.

There are two protocol-handshake exceptions, applied only to the copy Sonda
stores. **PostgreSQL passwords and AMQP SASL challenges and responses are
blanked in the tap** before anything reaches SQLite. Neither kind of capture can
be replayed faithfully without its original connection, and the alternative is
a live credential sitting in a plaintext file. The mechanism name remains, and
the upstream still receives the original bytes. Nothing else is touched, and no
setting adds to the list.

Two consequences follow, and neither is a bug report:

- **`sonda.db` is a file with credentials in it.** Treat it the way you would
  treat a log that captured an `Authorization` header. Do not commit it, do not
  attach it to a ticket, and think before copying it to another machine.
- **Sonda has no authentication.** It is a local development tool. It binds to
  `127.0.0.1` and assumes the only person who can reach it is the person running
  it.

## The certificate authority

The first time a service is set to terminate TLS, Sonda generates a certificate
authority and writes two files beside the database:

| | |
|---|---|
| `sonda-ca.pem` | the certificate. Public, and safe to hand around |
| `sonda-ca-key.pem` | **the private key.** Written `0600`, owner only |

**The key is the most dangerous thing Sonda holds.** `sonda.db` exposes the
credentials that happened to cross the wire while it was running; this key
signs a certificate for *any* name, so anyone who has it can impersonate any
site to every machine that trusts the authority. Treat it the way you treat an
SSH private key, not the way you treat a log file. It is never logged, never
returned by the API, never reachable over MCP, and never written into a
capture. `GET /api/tls/ca.pem` serves the certificate and there is no endpoint
that serves the key.

Two things follow from where it lives:

- **Copying `sonda.db` to another machine copies the key too** if you take the
  whole directory. Take the database on its own.
- **Deleting Sonda's directory does not untrust the authority.** If you trusted
  it system-wide, withdraw the trust first and delete the files second —
  otherwise the machine keeps trusting a root nobody can account for. The
  removal commands are in the certificate authority panel of the interface, in
  the `trust_certificate` MCP tool, and in `README.md`.

**Sonda never installs it.** No `certutil`, no `security add-trusted-cert`, no
writing into `/usr/local/share/ca-certificates`, no registry. Modifying a
machine's trust store is a decision to make deliberately, and a debugging tool
that did it quietly would be indistinguishable from malware. Sonda prints the
exact command for your platform and stops there.

The authority names itself and the machine it was made on — `Sonda local CA
(hostname)` — and expires after a year, so one forgotten in a trust store stops
mattering on its own.

## Not verifying an upstream

`insecure_skip_verify` stops Sonda checking an upstream's certificate. It is per
service, it is refused on anything that is not an `https://` upstream, and there
is deliberately no process-wide equivalent: "I trust this one self-signed
container" and "I trust anything" are not the same statement.

It is never quiet. The service is marked in the web interface, in the terminal's
channel rail and in `list_services`, and **every capture taken through it records
that it was not verified** — so a response read six months later still says
whether anyone ever checked who sent it.

## Do not expose it

Sonda is not hardened for a network. Anyone who can reach its port can read
every credential in every capture, replay any call against your services, and
turn on fault injection. There is no login to get past.

Concretely:

- Do not bind it to an address anyone else can reach, and do not publish its
  port from a container to a network you do not control.
- Do not run it in a shared or production environment.
- Inside a container Sonda binds `0.0.0.0`, because a published port can reach
  nothing else; the boundary is the port mapping instead. Keep the host side on
  `127.0.0.1` — the `docker run` line in `README.md` and `compose.yaml` both do.

## Windows autostart

`sonda autostart install` registers a Task Scheduler entry for the current user,
not a machine-wide service. It uses the user's interactive token at least
privilege, stores no password, never requests elevation, and starts only when
that user signs in. The task definition contains absolute paths to the launcher,
configuration, working directory and log; it does not contain captured data or
application credentials.

Every lifecycle command derives the expected task from trusted local inputs:
the current SID, the selected absolute configuration, the current launcher and
working directory, and the recomputed control and health identities. A task at
the deterministic name is neither replaced nor started, stopped, ended, or
deleted when its identity, metadata, action, principal, trigger, or safety
settings differ. Pass the original `-config PATH` to lifecycle commands after a
custom-path installation.

Installation refuses a non-loopback `api_listen` by default. Passing
`-allow-non-loopback` is an explicit acknowledgement that the API described
above will be reachable outside the workstation. This is not an authentication
mechanism and does not make network exposure safe.

The normal stop path uses a current-user named event tied to the task and
configuration, then follows the same context cancellation that Ctrl+C uses, so
buffered captures drain before SQLite closes. Only when graceful signaling
fails, the task still matches the canonical definition, and that expected event
still proves the managed process is active may Sonda ask Task Scheduler to end
the exact task. This fallback is abrupt and is reported as such. It never kills
processes by executable name or enumerates unrelated Sonda instances.

The task launcher is a verified Scoop shim only when the running executable is
that shim's exact `apps/sonda/current` target; an unrelated installed shim is
never selected or executed as a compatibility probe. Portable and locally built
binaries keep working only while their absolute path exists, and
`status` reports a moved or missing launcher. After moving one, restore its old
path or remove the task manually before reinstalling; Sonda does not treat a
changed action as its own. Removing the task leaves `sonda.yaml`, `sonda.db`, the
CA files and logs untouched; deleting or trusting those remains a separate user
decision.

## What MCP redacts, and what it cannot

The MCP server is the one place credentials are held back, because there the
answers leave the machine and land in whatever model an agent is driving.
`Authorization`, `Cookie`, `password` and their various spellings come back as
`[redacted by Sonda]`, in headers and inside bodies, and there is no setting to
turn that off. The web interface still shows everything, because there the
reader is the owner, except for the Postgres and AMQP handshake secrets that
were blanked before any surface could read them.

**Every rule is chosen by position in Sonda's own answer, not by what a field is
called inside a payload.** An answer has two kinds of field in it. Some are
Sonda's own — the path, the one-line Postgres summary, a trace node's detail,
the tree drawn as text, the `sql` and `params` of a decoded statement — and
Sonda knows what each of those *is*, so each gets a rule written for exactly
that field and reachable from nowhere else. The rest is captured content:
request and response bodies, frame payloads, decoded messages. Sonda knows
nothing about their shape, so there the only tool is name matching, and nothing
else runs there. A SQL scanner never sees prose, and a body that happens to hold
a field called `detail`, `sql` or `postgres` is left exactly as it was recorded.

Which endpoint produced an answer is what fixes those positions, and the tools
call the API by path, so the position is known before the bytes are parsed. An
endpoint with no schema written for it is treated as captured content from top
to bottom, which is the direction that cannot leak a rule into the wrong place.

**Inside captured content, redaction matches on the name, never on the value.**
Sniffing values sounds cleverer and fails both ways: it misses an opaque session
id and mangles a legitimate field that happens to look like base64. So the
question it can answer there is always "is this field called something
credential-like".

**Redaction runs over the whole payload before anything is shortened.** Bodies
come back cut at two thousand characters unless `detail` is asked for, and for a
while the cut happened first — so a statement longer than that reached the
credential gate already truncated, and the *default* answer was the leaky one.
The order is now fixed and pinned by a test: whatever is dropped for display was
already redacted.

What it reaches:

- **Header and JSON field names**, in any spelling — `api_key`, `apiKey`,
  `X-API-KEY` and `x-company-auth-token` all land on the same answer.
- **Inside a captured body**, when the body is itself JSON. A body is stored as
  one opaque string, so this is a second pass through the same walk.
- **Query-string parameters**, wherever a URL appears — the captured path, a
  `Location` redirect, a `Referer`, a link inside a body. Every `?` in the
  string is examined, not the first, and pairs are split on `;` as well as `&`.
  Only the value of a sensitive parameter is blanked; the path, the other
  parameters and any fragment stay, because the path is how a person recognises
  the call. Three names are treated as credentials here and nowhere else —
  `code` (the OAuth authorization code), `key` (a Google API key rides in
  `?key=`) and `sig`. As a field name `code` is the SQLSTATE of every Postgres
  error and an HTTP status besides, so putting it in the general list would
  blank the most useful field a failed capture has.
- **Postgres column values**, by aligning a `RowDescription` with the `DataRow`s
  after it: `SELECT api_key FROM tokens` puts the sensitive name in one message
  and the secret in the next, which no per-field rule can see on its own. A row
  carrying more values than the description had columns has the unaligned tail
  blanked as well.
- **String literals in a Postgres statement that names a credential**, plus the
  bind parameters of that statement. `INSERT INTO users (email, password)
  VALUES ('a', 'hunter2')` comes back with its structure intact and its literals
  blanked. A bind parameter is a position with no name, so when the statement it
  belongs to touches a credential, all of its parameters go.
- **A result row whose column was aliased.** `SELECT api_key AS k FROM tokens`
  describes a column called `k`, so alignment finds nothing. When the statement
  names a credential and none of the described columns does, the whole row is
  blanked — the same blunt rule the bind parameters follow. It costs the id and
  the email of a login lookup, which the web interface still shows.
- **The one-line Postgres summary**, which is the field a listing carries and
  therefore the one that reaches an agent on its first tool call, before it has
  asked for detail. A statement summary has its literals blanked; an error
  summary keeps its SQLSTATE and loses the server's message, because Postgres
  echoes the offending value there (`invalid input syntax for type uuid:
  "hunter2"`) in forms with no structure worth mining. This runs at the two
  positions the line actually occupies — `postgres_summary` in a listing or in a
  capture, and the `detail` of a trace node that says it is a Postgres one — and
  is unreachable anywhere else.
- **The same line inside the drawn tree.** `trace_call` returns the tree twice,
  as structure and as a block of text, and every node's detail is repeated in
  the drawing. The drawing is not scanned for it: each node reports what its own
  line became and those exact strings are substituted in, so the substitution
  reaches every node at every depth. Scanning it is what left the previous
  version leaking — the scanner recognised a summary by counting the words
  before the first colon, and the branch character `├─` counts as a word, so it
  fired on the root and on the last child and nowhere in between.
- **The `message`, `detail` and `hint` of a Postgres error**, for the same
  reason, when the statement or the text itself names a credential.
- **Both sides of a changed credential in a diff.** `diff_calls` addresses a
  changed field by a path — `{"path":"user.password","a":…,"b":…}` — so there
  the field's *name* is a value and the keys around it are `path`, `a` and `b`.
  Name matching reads keys, so it went straight past this, and `diff_calls` is
  the tool an agent reaches for when a login worked once and then did not. When
  any segment of that path names a credential, both sides are blanked; the path
  itself stays, because knowing *which* field changed is the answer.
- **The verbatim copy of a decoded capture.** A Postgres session, a WebSocket,
  an event stream, a gRPC call and an AMQP unit each carry the same bytes twice — decoded
  under their own view, and verbatim as what crossed. In the second copy the
  statement, the frame payload, the event and the protobuf field are all
  legible and everything above counts for nothing, and none of them can be
  blanked selectively: each is a length followed by a run of bytes at an
  arbitrary offset, with no quoting to scan for. So over MCP the verbatim copy
  is replaced wherever the decoded view beside it genuinely replaces it, side by
  side: `sent` stands in for the request, `received` for the response, the
  events for the response of a stream. **A view that decoded nothing replaces
  nothing and the bytes stay.** A 502 HTML page served under
  `text/event-stream` is an event stream by its content type and holds no
  events; dropping the body there left a reader with no error page and no
  bytes, which is worse than either. The same holds for the *request* of an
  event stream, which nothing decodes at all, for a gRPC side carrying a
  compressed frame — the encoding is negotiated in a header Sonda does not hold
  — and for any body that yielded no messages. The sizes always stay, and the
  web interface still shows the stream.

What it does **not** reach, and will not:

- **A secret under a name that says nothing.** `{"value": "sk_live_…"}`,
  `{"data": "…"}`, a column called `col3`. There is no name to match.
- **A service's own error message.** The `error` and `grpc_message` of a
  summary, and a trace node's detail when it is one of those rather than a
  Postgres summary, are returned as the service wrote them. They are prose, and
  the alternatives are both worse: reading prose as SQL truncates it at the
  first apostrophe, and blanking any line that happens to name a credential
  loses `Internal: couldn't refresh the session cookie` — the message, in the
  tool that exists to show failures. If your services put tokens in their error
  strings, those tokens come out.
- **Anything at a position no schema was written for.** Bodies are captured
  content wherever they turn up, so name matching always runs; what does not run
  at an unrecognised endpoint is a rule that depends on knowing the field. The
  answers of `list_services`, `schema_status`, `diagnose_silence`,
  `contract_drift`, `trust_certificate` and `replay_call` carry no captured
  payload, which is why they need none. A tool added later against a new
  endpoint gets the name matching and nothing more, and the rule for its own
  fields has to be written with it.
- **A Postgres statement that names no credential.** `INSERT INTO t VALUES
  ('sk_live_…')` keeps its literal, and the bind parameters of an ordinary
  statement are returned in the clear — deliberately, because the values are
  usually the reason the capture was opened. The same holds for an aliased
  column when the statement gives nothing away: `SELECT api_key AS k FROM t`
  reaches the alias rule above, `SELECT c1 AS k FROM t` does not.
- **A `DataRow` with no `RowDescription` before it** in the same capture, and a
  `Bind` whose `Parse` is not in the same capture. The alignment has nothing to
  align against, and a row with no description at all is returned whole rather
  than blanked.
- **A credential-shaped query parameter under a name nobody listed.**
  `?code_verifier=` and `?assertion=` are secrets and are returned in the clear.
  The list is a list.
- **A body that is not JSON.** It is returned exactly as it was recorded — as
  text, or as base64 when the bytes are not valid UTF-8 — because there are no
  field names in it to match. A form post, a CSV, a protobuf: whatever is in
  them comes out.
- **A protobuf field decoded without a schema.** With a descriptor set or with
  the service's own reflection, a gRPC message comes back with real field
  names, and ordinary name matching redacts it like any other payload — that
  case is closed, in the decoded view and in the verbatim copy alike. With
  neither, the wire format carries numbers and not names: `{"number": 1,
  "value": "sk_live_…"}`. There is nothing to match on, and the value is
  returned in the clear. Blanking every unnamed field would empty the one view
  that exists precisely for when no schema could be found, so it is not done.
  `schema_status` says which of the two you are getting, per service, and why.
- **Personal data.** An email address, a name, an address and a card number are
  not credentials and are returned whole.

Over-redaction is the direction this errs in. The cost of blanking one field too
many is that a person reads it in the interface instead; the cost of the other
mistake is a production token in someone else's model.

**If your captures hold something the rules above cannot see, do not point an
agent at them.** Redaction is a floor, not a guarantee.

## Local schema files over MCP

`upload_schemas` can accept a local descriptor `path` only in the
`sonda mcp` stdio adapter. That local process accepts up to 32 MiB and forwards
the bytes to the existing descriptor upload API. The HTTP MCP endpoint does not
advertise `path`, has no filesystem reader, and rejects the argument even if a
stdio-capable server value were accidentally reused there. Remote clients must
send `filename` and `content_base64`; no MCP request can ask the Sonda HTTP
server to read an arbitrary path.

## Reporting a vulnerability

Open a [security advisory](https://github.com/NicolasCondezaR/sonda/security/advisories/new),
not a public issue.

What is in scope: anything that lets code or a page outside Sonda read captures
or drive it — a bypass of the `Origin` check on the MCP endpoint, a path that
leaks a redacted field, a way to make the proxy forward somewhere it was not
configured to.

What is not: that the database is unencrypted, that there is no authentication,
that exposing the port is dangerous, or that a certificate authority you chose
to trust can sign for any name. Those are documented above and are properties of
a local tool, not defects. A path that leaks the CA private key, or that skips
upstream verification without the target being configured for it, very much is.

## Supported versions

The latest release. Sonda is pre-1.0 and fixes land in the next tag rather than
being backported.
