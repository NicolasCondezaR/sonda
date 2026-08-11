# Security

## What Sonda holds

Sonda stores the bytes that crossed the wire, and in a real system that means
**bearer tokens, session cookies, API keys and personal data**, in a SQLite file
with no encryption. Nothing is redacted on the way in, and that is deliberate: a
capture that was altered is no longer what was sent, which would break both the
fidelity the tool is built on and replay along with it.

There is exactly one exception. A **PostgreSQL password is blanked in the tap**,
before anything reaches the database — a statement cannot be replayed anyway, so
nothing is lost by not keeping it, and the alternative is a live credential
sitting in a plaintext file. Nothing else is touched, and no setting adds to the
list.

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

## What MCP redacts, and what it cannot

The MCP server is the one place credentials are held back, because there the
answers leave the machine and land in whatever model an agent is driving.
`Authorization`, `Cookie`, `password` and their various spellings come back as
`[redacted by Sonda]`, in headers and inside bodies, and there is no setting to
turn that off. The web interface still shows everything, because there the
reader is the owner.

**Redaction matches on the name, never on the value.** Sniffing values sounds
cleverer and fails both ways: it misses an opaque session id and mangles a
legitimate field that happens to look like base64. So the question it can answer
is always "is this field called something credential-like", and everything below
follows from that.

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
  asked for detail. It is a plain string, so nothing about it says "this is
  SQL": it is gated separately. A statement summary has its literals blanked; an
  error summary keeps its SQLSTATE and loses the server's message, because
  Postgres echoes the offending value there (`invalid input syntax for type
  uuid: "hunter2"`) in forms with no structure worth mining.
- **The `message`, `detail` and `hint` of a Postgres error**, for the same
  reason, when the statement or the text itself names a credential.
- **The raw stream of a Postgres capture.** A call carries the same
  conversation twice — decoded under `postgres`, and verbatim as the bytes that
  crossed. In the second copy the statement, its bind parameters and every row
  value are legible and everything above counts for nothing, and it cannot be
  blanked selectively: a Postgres value is a length followed by a run of bytes
  at an arbitrary offset, with no quoting to scan for. So over MCP the verbatim
  copy is replaced whole and the decoded one is what an agent reads. The sizes
  stay, and the web interface still shows the stream.

Redaction is scoped to where the protocol actually is. The Postgres pass reads
neighbouring messages, and it only does so inside a capture's `postgres` view
and only across objects that carry a pgwire message `kind`. A captured body that
happens to hold `sql`, `columns`, `values` or `params` keys is left exactly as
it was recorded: for a body, the stored bytes are the record, and that is the
one surface where the reader cannot check them against the web interface.

What it does **not** reach, and will not:

- **A secret under a name that says nothing.** `{"value": "sk_live_…"}`,
  `{"data": "…"}`, a column called `col3`. There is no name to match.
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
- **The raw copy of a WebSocket, event-stream or gRPC capture.** Every one of
  those is served the way Postgres is, decoded *and* verbatim, and only the
  Postgres verbatim copy is replaced. The decoded frames and messages are
  redacted; the bytes beside them are not. This is a known gap, not a claim.
- **Personal data.** An email address, a name, an address and a card number are
  not credentials and are returned whole.

Over-redaction is the direction this errs in. The cost of blanking one field too
many is that a person reads it in the interface instead; the cost of the other
mistake is a production token in someone else's model.

**If your captures hold something the rules above cannot see, do not point an
agent at them.** Redaction is a floor, not a guarantee.

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
