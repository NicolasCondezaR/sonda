# Security

## What Sonda holds

Sonda stores the bytes that crossed the wire, and in a real system that means
**bearer tokens, session cookies, API keys and personal data**, in a SQLite file
with no encryption. Nothing is redacted on the way in, and that is deliberate: a
capture that was altered is no longer what was sent, which would break both the
fidelity the tool is built on and replay along with it.

Two consequences follow, and neither is a bug report:

- **`sonda.db` is a file with credentials in it.** Treat it the way you would
  treat a log that captured an `Authorization` header. Do not commit it, do not
  attach it to a ticket, and think before copying it to another machine.
- **Sonda has no authentication.** It is a local development tool. It binds to
  `127.0.0.1` and assumes the only person who can reach it is the person running
  it.

## Do not expose it

Sonda is not hardened for a network. Anyone who can reach its port can read
every credential in every capture, replay any call against your services, and
turn on fault injection. There is no login to get past.

Concretely:

- Do not bind it to `0.0.0.0` or publish its port from a container to a network
  you do not control.
- Do not run it in a shared or production environment.
- The container image publishes port 9000; keep the host side on `127.0.0.1`.

The MCP server is the one place credentials are held back, because there the
answers leave the machine and land in whatever model an agent is driving.
`Authorization`, `Cookie`, `password` and their various spellings come back as
`[redacted by Sonda]`, in headers and inside bodies, and there is no setting to
turn that off.

## Reporting a vulnerability

Open a [security advisory](https://github.com/NicolasCondezaR/sonda/security/advisories/new),
not a public issue.

What is in scope: anything that lets code or a page outside Sonda read captures
or drive it — a bypass of the `Origin` check on the MCP endpoint, a path that
leaks a redacted field, a way to make the proxy forward somewhere it was not
configured to.

What is not: that the database is unencrypted, that there is no authentication,
or that exposing the port is dangerous. Those are documented above and are
properties of a local tool, not defects.

## Supported versions

The latest release. Sonda is pre-1.0 and fixes land in the next tag rather than
being backported.
