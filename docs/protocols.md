[← Docs](README.md)

# Protocols

## gRPC

Set `protocol: grpc` on a target and point it at the service's port. Sonda
speaks cleartext HTTP/2 to it, forwards the call untouched — trailers included,
which is where gRPC reports whether the call actually worked — and decodes the
messages when it can find a schema.

```bash
# through Sonda, not straight at the service
grpcurl -plaintext -d '{"order_id":"ORD-777"}' 127.0.0.1:9201 demo.v1.Orders/GetOrder

# only the calls that failed, across HTTP status, gRPC status and transport errors
curl 'http://127.0.0.1:9000/api/calls?failed=true'

# and the contrast: only the ones that did not. Leave failed out for both
curl 'http://127.0.0.1:9000/api/calls?failed=false'
```

### Where the schema comes from

Three sources, tried in order, each degrading into the next:

1. **Reflection.** If the service serves it, Sonda asks and needs nothing else.
   It asks the service directly rather than through the proxy, so its own
   bookkeeping does not end up in your timeline.
2. **A descriptor set on disk.** For services without reflection, compile the
   protos and point at the result:
   ```bash
   buf build -o descriptors.binpb
   # or: protoc --include_imports --descriptor_set_out=descriptors.binpb path/to/*.proto
   ```
   ```yaml
   - name: orders
     listen: 127.0.0.1:9201
     upstream: http://127.0.0.1:50051
     protocol: grpc
     descriptor_set: ./descriptors.binpb
     reflection: false
   ```
3. **The wire format itself.** With no schema at all, messages still come back
   as numbered fields with guessed types, nested structure intact:
   ```json
   [
     {"number": 1, "type": "string",  "value": "ORD-777"},
     {"number": 2, "type": "varint",  "value": 3, "note": "could be an integer, a bool or an enum"},
     {"number": 3, "type": "message", "value": [...], "note": "guessed to be a nested message"}
   ]
   ```
   Guesses are labelled as guesses. On the wire a varint really could be an
   int32, a bool or an enum, and saying so is the difference between a useful
   view and a misleading one.

`GET /api/schemas` reports which source each target resolved to, and the reason
when none did. That is the endpoint to check first when field names are missing.

To find out whether a service serves reflection at all:

```bash
grpcurl -plaintext <host>:<port> list
```

### What gRPC support does and does not cover

- Unary, server streaming and client streaming are all captured, with every
  message on both sides — not just the first.
- The status comes from the trailers, or from the headers on a trailers-only
  response, which is how a server reports an error with nothing to send. A
  failed gRPC call still carries HTTP 200; the listing shows both.
- `grpc-message` is percent-decoded, so an error in Spanish reads as Spanish.
- Compressed messages are reported as compressed and not decoded. The encoding
  is negotiated per call and guessing at it would produce confident garbage.
- A `grpc` target still forwards the plain HTTP that shares the port — health
  and metrics endpoints — and classifies it by content type rather than by how
  the target was configured.
- Reflection calls made *through* the proxy are captured like any other traffic.
  Filter them out with `?path=demo.v1` or whatever matches your own services.

## TLS

Two different problems wear the same three letters, and Sonda answers them
separately.

### The upstream speaks TLS

Declare it with the scheme it has and nothing else changes:

```yaml
- name: payments
  listen: 127.0.0.1:9103
  upstream: https://api.payments.example.com
  protocol: http
```

The certificate is verified the way any other client would verify it. A gRPC
target behind TLS works the same way — it negotiates HTTP/2 over ALPN instead of
h2c — and an upstream written without a port gets the one its scheme implies.

An upstream whose certificate does not verify answers 502 with the reason, and
the capture records the transport error. That is deliberate: a proxy that
quietly accepted any certificate would make every "verified" reading in the tool
worthless.

### The client speaks TLS

Some clients refuse `http://` outright. `tls: true` makes Sonda answer that port
with a certificate of its own:

```yaml
- name: web-api
  listen: 127.0.0.1:9104
  upstream: http://127.0.0.1:3000
  protocol: http
  tls: true
```

The caller is then pointed at `https://127.0.0.1:9104`, and the interface hands
over that exact line. The certificate is minted on the fly for whatever name the
client asked for — the SNI name, or the address it connected to when it sent
none — cached in memory per name, and signed by an authority Sonda generates the
first time one is needed.

Postgres is the exception. A database negotiates encryption inside its own
protocol rather than before it, so a TLS listener in front of it would be
answering a handshake no client sends. Sonda refuses the flag there rather than
accepting it and ignoring it.

### Trusting the certificate authority

**Sonda does not install anything.** It writes two files beside the database —
`sonda-ca.pem` and `sonda-ca-key.pem`, the key owner-only — prints what to run,
and stops. Modifying your machine's trust store is your decision to make
deliberately; a debugging tool that did it quietly would be indistinguishable
from malware.

The commands are printed the first time the authority is actually needed — when
a service with `tls: true` first starts, which is not the same as the first run:
a Sonda with no TLS target never creates one and prints nothing. They are also
shown in the interface's certificate authority panel, and returned by the
`trust_certificate` MCP tool. The narrow option is usually the right one,
because it trusts nothing else on the machine and leaves nothing to undo:

```bash
curl --cacert ./sonda-ca.pem https://127.0.0.1:9104/
NODE_EXTRA_CA_CERTS=./sonda-ca.pem npm start
SSL_CERT_FILE=./sonda-ca.pem go run ./cmd/whatever
REQUESTS_CA_BUNDLE=./sonda-ca.pem python app.py
```

Every one of those lines names a path, and when Sonda runs in a container the
path it prints is a path inside the container — there is no `/data` on your
machine. Get the file onto your own disk first, then use that path instead:

```bash
docker compose cp sonda:/data/sonda-ca.pem ./sonda-ca.pem
# or, without Docker in the way
curl -o sonda-ca.pem http://127.0.0.1:9000/api/tls/ca.pem
```

An agent has neither: `trust_certificate` returns the certificate itself in
`certificate_pem`, so it can write the file and hand over the path it wrote.

For the whole machine — which is what a browser needs — run the line for your
platform yourself:

```bash
# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ./sonda-ca.pem
# Windows
certutil -user -addstore Root .\sonda-ca.pem
# Linux (Debian, Ubuntu)
sudo cp ./sonda-ca.pem /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates
# Linux (Fedora, RHEL)
sudo cp ./sonda-ca.pem /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust
```

Firefox keeps its own store: **Settings → Privacy & Security → Certificates →
View Certificates → Authorities → Import**, then tick *Trust this CA to identify
websites*.

And to take it back out again — withdraw the trust first, delete the files
second, or the machine keeps trusting a root nobody can account for:

```bash
# macOS
sudo security delete-certificate -c "Sonda local CA (your-hostname)" /Library/Keychains/System.keychain
# Windows — the serial is printed with the certificate and shown in the interface
certutil -user -delstore Root <serial>
# Linux (Debian, Ubuntu)
sudo rm /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates --fresh
# Linux (Fedora, RHEL)
sudo rm /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust

rm ./sonda-ca.pem ./sonda-ca-key.pem
```

The authority names itself and the machine it was made on — `Sonda local CA
(hostname)` — so it is findable in a trust store a year later, and it expires
after a year, so one that is forgotten stops mattering on its own. The private
key is never logged, never returned by the API, never reachable over MCP and
never written into a capture; `SECURITY.md` says what that means for copying the
database around.

### Not verifying an upstream

The developer case this exists for is a service with a self-signed certificate.
There is exactly one way to skip the check, and it is per service:

```yaml
- name: staging
  listen: 127.0.0.1:9105
  upstream: https://staging.internal:8443
  protocol: http
  insecure_skip_verify: true
```

There is no process-wide switch and there is not going to be: "I trust this one
container" and "I trust anything" are not the same statement. Sonda refuses the
flag on a plaintext upstream, where it would mean nothing while still reading as
unverified everywhere.

And it is never quiet. The service is marked in the web interface, in the
terminal's channel rail and in `list_services`, and every capture taken through
it carries `upstream_insecure` — so a response read months later still says
whether anyone ever checked who sent it.

## PostgreSQL

Every other protocol Sonda captures is one hop between services. Postgres is the
hop underneath: it puts the SQL a request actually ran in the field under the
HTTP call that caused it, one row per statement, and an N+1 stops being a theory
and becomes forty rows under one handler.

A database target is declared like any other service, with `protocol: postgres`
and an upstream that names the transport:

```yaml
  - name: orders-db
    listen: 127.0.0.1:9301
    upstream: postgres://127.0.0.1:5432
    protocol: postgres
```

Then point the application's DSN at the listen address, keeping its own user,
password and database name:

```
DATABASE_URL=postgres://app:secret@127.0.0.1:9301/orders
```

**The upstream carries no credentials, and Sonda refuses one that does.** It
forwards the client's own handshake untouched, so it has no use for a user or a
password — and a password in the configuration would be a password written into
`sonda.db`.

### The password never reaches the capture

This is the one place Sonda rewrites what it keeps, and the reason is that the
alternative cannot be fixed later. A startup exchange carries the password. If
the raw bytes were stored as they arrived, the secret would sit in `sonda.db` in
plaintext and could reach an agent through MCP — whose redaction reads field
names, query strings and the shape of a Postgres exchange, and a startup
handshake is none of those: the password is a length-prefixed run of bytes in a
TCP stream, under no name at all.

So the credential bytes are blanked in the tap, as they go past, before anything
is stored: the PasswordMessage and SASL response bodies, the server's
authentication challenges, and the cancellation key in both directions. They are
replaced by filler of the same length, so every length field in the stream stays
true and the capture still reads back as a conversation. What survives is that
an authentication happened and by which mechanism — `sasl`, `md5_password`,
`cleartext_password` — which is the part a reader is entitled to.

**What is forwarded is untouched.** The rewrite applies to the copy Sonda keeps,
never to the bytes the database receives; the real password reaches the server
and the login works. Both halves of that are covered by tests.

### What a capture is

- **One capture is one statement, not one connection.** The protocol gives the
  boundary away: a simple query is `Q` → results → `ReadyForQuery`, an extended
  one is Parse/Bind/Describe/Execute/Sync → results → `ReadyForQuery`, and the
  `Z` ends the cycle in both. Each row carries the SQL, the values bound to it,
  what the server answered — the command tag, the row count or the error — and
  the timing of that statement alone.
- **The SQL hangs under the request that ran it**, and nothing new correlates
  them. Sonda already arranges calls into a tree by containment: a call that
  begins no earlier and ends no later than another is its child, and a statement
  run during an HTTP request is contained in it by definition. That only works
  because a capture is timed from the statement rather than from the connection,
  which is the whole reason the split exists — behind a pool one connection
  carries hours of an application's SQL, and hours of SQL is not something you
  can hang off a request.
- **The connection's opening rides its first statement.** The startup
  parameters, the mechanism that was demanded, the server's settings: they
  happen once, and they are context for the first thing the connection ran
  rather than a row of their own — a row with no SQL in it would be noise on
  every pooled connection. Dropping the fact that an authentication happened is
  not something a debugger gets to do, so a connection that authenticated and
  then ran nothing does get its own row, with the method `SESSION`.
- **The path is the database**, read off the startup message rather than
  invented, and carried onto every statement of the connection — only the first
  one holds the message it came from. The method is `STATEMENT`. There is no
  HTTP status, and none is shown.
- **The listing carries the statement itself** and how it ended
  (`SELECT id FROM orders WHERE total > 100 -> SELECT 12`), or the error if
  there was one. Every result of a cycle is named, not just the first: a `COPY`
  and a multi-statement simple query both answer once with several.
- **A statement that never finished is recorded and says so.** The connection
  died mid-query, or the capture ended while the statement was still in flight:
  the row is written with what crossed and an error saying no `ReadyForQuery`
  ever arrived. Reporting it as a success would be a lie, and dropping it would
  lose exactly the statement worth having.
- **The statement is searchable in full** — the SQL, the values bound to it, the
  command tags and the server's complaint — not only the summary line.
- **The inspector shows the messages**: the statements, their bind parameters
  with `NULL` distinguished from the empty string, the columns described, the
  command tags, the transaction status, and any server error with its SQLSTATE,
  detail and hint. Data rows are counted rather than drawn one by one.

**A SQL error is a failure.** It arrives as an `ErrorResponse` inside the stream
with no status code anywhere, so a tool that reads transports alone would show
it as a healthy statement. Sonda counts it as a fault everywhere the question is
asked: the fault filter, the channel rail, the field's fault marks, the terminal,
the trace tree and `recent_failures` over MCP. It is the same problem gRPC and
GraphQL have, answered the same way.

**A statement cannot be replayed.** It belongs to a connection, a session and a
transaction that are gone, and sending the SQL again would run it somewhere
else. Every surface refuses, including the API itself, so an agent gets the same
answer the browser does.

## AMQP 0-9-1

Declare RabbitMQ as an `amqp` target and point the client at Sonda, keeping the
client's own user, password and virtual host:

```yaml
  - name: events
    listen: 127.0.0.1:9401
    upstream: amqp://127.0.0.1:5672
    protocol: amqp
```

```text
AMQP_URL=amqp://app:change-me@127.0.0.1:9401/orders
```

The configured upstream must not contain credentials. Sonda forwards the
client's own handshake byte for byte and has no use for a second copy of the
password in configuration.

Use `amqps://` when the broker hop speaks TLS. Listener TLS is independent:

```yaml
  - name: secure-events
    listen: 127.0.0.1:9402
    upstream: amqps://rabbitmq.internal:5671
    protocol: amqp
    tls: true
```

Here Sonda verifies the broker certificate and the caller uses
`amqps://127.0.0.1:9402`. As with HTTPS, the caller must trust Sonda's local
authority. `insecure_skip_verify: true` is accepted only for this one
`amqps://` target and is recorded everywhere; it is not a global switch.

### What one capture means

AMQP is a bidirectional session rather than a request/response protocol, so
Sonda stores useful, direction-specific units instead of inventing pairs:

- `basic.publish`, `basic.return`, `basic.deliver` and `basic.get-ok` include
  their content header and body frames for that channel.
- Handshake, topology, acknowledgement and close methods stand alone. Heartbeat
  frames are forwarded but omitted from storage.
- The list carries the AMQP method and a searchable route, queue, virtual host
  or channel. Message text and broker replies are also indexed.
- Broker `close` and `return` errors with reply codes of 300 or greater are
  faults in the API, web field, terminal and `recent_failures`.

`GET /api/calls/{id}` exposes the decoded frames under `amqp.sent` and
`amqp.received`, plus `sent_incomplete` and `received_incomplete`. The web
inspector and TUI render those same frames. MCP `get_call` returns that decoded
view and removes the redundant raw copy; `search_calls` accepts
`protocol: "amqp"` and the same method, path and text filters as other captures.

### Credentials and limits

What the broker receives is untouched. In the copy stored by Sonda, SASL
challenges and responses from `connection.start-ok`, `connection.secure` and
`connection.secure-ok` are zeroed while the selected mechanism name remains.
If a connection ends in the middle of a frame that could contain a credential,
Sonda records its size and error but stores none of those partial raw bytes.

A frame larger than the 16 MiB capture parser limit is still forwarded, but is
stored only as size and error metadata. `max_body_bytes` independently limits
how many bytes of an otherwise valid unit reach SQLite; forwarding is still
complete. These captures cannot be replayed, stubbed or fault-injected: doing so
would require recreating a connection and channel state that no longer exists.
Sonda supports AMQP **0-9-1**, not AMQP 1.0, and does not reconstruct broker
transactions or pair publishes with deliveries.

## GraphQL

A GraphQL client sends every operation as the same POST to the same path, so a
service that speaks GraphQL arrives in the field as one call repeated a hundred
times and the timeline stops being useful for it. Sonda reads the operation out
of the body and labels the call with it: `POST /graphql · mutation Pay`.

- **The document is detected by its body, not its path.** A POST whose JSON
  carries a `query` string is a GraphQL request wherever it was sent — behind a
  gateway prefix, on `/api`, or on `/graphql`.
- **A batch is every operation in it.** Clients send arrays of operations as one
  request; reading only the first would hide the rest.
- **The inspector shows the operation**: its type and name, the top-level fields
  it asked for, the variables it sent, and every error the response carried with
  its path and its `extensions.code`. The raw bodies stay alongside it.

**A GraphQL error is a failure.** The server answers HTTP 200 with an `errors`
array, so a tool that reads the status code alone shows the thing you came to
find as a success. Sonda counts it as a fault everywhere the question is asked:
the fault filter, the channel rail's counts, the field's fault marks, the
terminal, the trace tree and `recent_failures` over MCP. This is the same
problem gRPC has, and it is answered the same way.

Sonda reads enough of the document to name the operation and no more. It is not
a parser: it does not validate, resolve fragment spreads into their fields, or
know your schema. A response that is not JSON — cut by the body cap, or an error
page from something in front of the service — is reported as unreadable rather
than as a call with no errors.

## Sockets and event streams

A WebSocket stops being requests and responses the moment its handshake
succeeds, so Sonda holds both directions of it itself rather than letting the
reverse proxy relay bytes nobody sees. What is stored is the raw frame stream
per direction, and the frames are read back out when you look — the same
arrangement gRPC streaming already uses.

- The handshake goes through **verbatim**: the key, the subprotocols and the
  negotiated extensions are what the two ends are agreeing on, and changing any
  of them would make the recorded conversation a different one.
- Client frames are **unmasked** for display. The mask is transport scaffolding
  the receiving end removes before anything reads the payload; the key is kept
  so the frame can still be reproduced exactly.
- Text, binary, ping, pong, fragments and close all show as what they are, and a
  close frame reports its code and reason — usually the answer to why the socket
  stopped working.
- An upgrade the service **refuses** is relayed as the ordinary response it is,
  so you can read why.

**A socket is captured when it closes**, not while it is open: it is one
exchange, and the exchange is not over. A long-lived socket is bounded by the
same body cap as any other capture, and says how much it did not keep.

Server-sent events need none of that — the response is ordinary HTTP — so they
are captured as they always were and split back into their events for display,
comments and keepalives dropped, a partial last event shown as partial.

