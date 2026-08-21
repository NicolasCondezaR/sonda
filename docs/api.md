[← Docs](README.md)

# API

| Method and path | Purpose |
|---|---|
| `GET /api/calls` | List captures, newest first. Filters: `target`, `method`, `path`, `status`, `protocol`, `grpc_status`, `failed`, `q`, `since`, `until`, `limit`, `before_id`. `failed=true` is only the failures, `failed=false` only the calls that did not fail, and leaving it out is both. |
| `GET /api/calls/{id}` | One capture with headers and bodies, plus the protocol-specific decoded view (`grpc`, `socket`, `stream`, `graphql`, `postgres` or `amqp`) when applicable. |
| `GET /api/targets` | The configured targets. |
| `GET /api/schemas` | Per gRPC target: which schema source resolved, or why none did. |
| `POST /api/calls/{id}/replay` | Send the call again, optionally onto another channel. |
| `GET /api/diff?a=&b=` | Structural comparison of two calls. |
| `GET /api/trace?call=` | The whole request a call belonged to, as a tree. |
| `GET /api/stub` | Which services are answering from recordings. |
| `POST /api/stub` | Turn stubbing on or off for a service, or clear it. |
| `GET /api/faults` | Which services are being broken on purpose, and how. |
| `GET /api/drift?target=` | Whether an endpoint still answers the shape it used to. |
| `POST /api/faults` | Set or clear a fault rule. |
| `GET /api/projects` | Projects, their services, and what is really listening. |
| `POST /api/projects` | Create one. `PATCH`/`DELETE /api/projects/{id}` rename and remove. |
| `POST /api/projects/{id}/activate` | Close the current project's ports and open this one's. |
| `POST /api/projects/deactivate` | Close every port. Nothing is deleted and activating brings it all back. |
| `POST /api/projects/{id}/descriptor` | Upload the compiled schemas for the whole project. |
| `POST /api/projects/{id}/services` | Add or update a service. `DELETE /api/services/{id}` removes one. |
| `POST /api/discover` | Read services out of a `.env` or compose file without saving anything. |
| `GET /api/runtime` | Which project is active and what is really listening, including how many connections each port has accepted. |
| `GET /api/diagnose` | Why nothing is being captured, per service. Reads only what Sonda already knows and touches no network. |
| `POST /api/diagnose` | The same report, plus one TCP dial to each upstream. A side effect, which is why it is not on the `GET`. |
| `GET /api/tls` | The certificate authority: the certificate itself in `certificate_pem`, the exact commands to trust it and to remove it, and — when Sonda is in a container — the `docker cp` that copies the file out. Never the private key. |
| `GET /api/tls/ca.pem` | Download the CA certificate. Useful when Sonda runs in a container. |
| `GET /api/stats` | Capture count, time span, and calls dropped under load. |
| `GET /api/stream` | Server-sent events: every capture the moment it is stored. What the live field reads. |
| `GET /health` | Liveness. |

The listing deliberately carries no bodies — a few hundred calls with payloads
attached is unusable. Bodies come from the detail endpoint, as `text` when the
content is valid UTF-8 and as `base64` when it is not. The API never guesses:
it reports what the bytes are.

`q` searches paths and text payloads. It is treated as a literal phrase, so
`"sku":"ABC-9"` and `/v1/orders` work as typed instead of being read as query
operators.

