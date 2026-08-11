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

What it reaches:

- **Header and JSON field names**, in any spelling — `api_key`, `apiKey`,
  `X-API-KEY` and `x-company-auth-token` all land on the same answer.
- **Inside a captured body**, when the body is itself JSON. A body is stored as
  one opaque string, so this is a second pass through the same walk.
- **Query-string parameters**, wherever a URL appears — the captured path, a
  `Location` redirect, a `Referer`, a link inside a body. Only the value of a
  sensitive parameter is blanked; the path, the other parameters and any
  fragment stay, because the path is how a person recognises the call.
- **Postgres column values**, by aligning a `RowDescription` with the `DataRow`s
  after it: `SELECT api_key FROM tokens` puts the sensitive name in one message
  and the secret in the next, which no per-field rule can see on its own.
- **String literals in a Postgres statement that names a credential**, plus the
  bind parameters of that statement. `INSERT INTO users (email, password)
  VALUES ('a', 'hunter2')` comes back with its structure intact and its literals
  blanked. A bind parameter is a position with no name, so when the statement it
  belongs to touches a credential, all of its parameters go.

What it does **not** reach, and will not:

- **A secret under a name that says nothing.** `{"value": "sk_live_…"}`,
  `{"data": "…"}`, a column called `col3`. There is no name to match.
- **A Postgres statement that names no credential.** `INSERT INTO t VALUES
  ('sk_live_…')` keeps its literal, and the bind parameters of an ordinary
  statement are returned in the clear — deliberately, because the values are
  usually the reason the capture was opened.
- **A `DataRow` with no `RowDescription` before it** in the same capture, and a
  `Bind` whose `Parse` is not in the same capture. The alignment has nothing to
  align against.
- **Binary values and non-JSON bodies.** Bytes are reported by size only, so
  there is nothing to redact — and nothing decoded either.
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
