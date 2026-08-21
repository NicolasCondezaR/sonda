[← Docs](README.md)

# Projects and configuration

## Projects

A project groups the services of one system — a monorepo, a side project,
whatever is being worked on today — and everything about it is configured from
the interface. Press **PROJECTS**.

The grouping is not filing for its own sake. It carries the two things that are
shared across a system's services and would otherwise be repeated on each one:

- **One descriptor set for the whole project.** Uploaded, not referenced by
  path, so it travels with the database when it is copied to another machine.
- **One answer to "are these ports open".** Only the active project listens, so
  two projects can claim the same port without colliding, and switching closes
  one set and opens the other without restarting anything.

Captures are tagged with the project they were taken under, so switching does
not pour one system's traffic into another's field.

### Import instead of typing

Setting up fifteen services by hand is how a tool like this gets abandoned after
one afternoon. The addresses are already written down somewhere, so
**IMPORT FROM A FILE** reads them: a `.env` full of `*_URL` entries, or a
compose file with published ports.

Every entry comes back with the line it was found on, and with its suggested
port already probed, so a wrong reading or a clash is visible before anything is
saved. Nothing is added until you say so.

```
+  ms-auth       grpc  http://localhost:50052  127.0.0.1:9152  port already in use
+  ms-billing    grpc  http://localhost:50067  127.0.0.1:9167  line 3: MS_BILLING_GRPC_URL
+  ms-executive  grpc  http://localhost:50064  127.0.0.1:9164  line 2: MS_EXECUTIVE_GRPC_URL
```

Database URLs, message brokers, callback URLs and anything else that is not a
service to call are left out. A list with a connection string in it is worse
than a list missing an entry: the first gets saved and proxied, the second gets
noticed.

### The one step no screen removes

Sonda is an explicit proxy. It sees nothing until whoever makes the call is
told to call it instead — no amount of configuration screen changes that, because
the caller decides where its requests go.

So each service hands over the exact line, ready to copy:

```
point the caller here:  MS_AUTH_GRPC_URL=127.0.0.1:9152
```

Restart the caller with that in its environment and its traffic appears in the
field. Nothing on disk changes, and dropping the variable puts it back.

The name is the one the address was read from when the project was imported —
`MS_AUTH_ADDR`, `MS_AUTH_HOST`, whatever the file actually says. It is only
derived from the service and its protocol when Sonda has no record of a name,
which is a service added by hand or read from a compose file: a guessed name
served beside the real one is a line that sets a variable nothing reads.

### The configuration file

`sonda.yaml` still carries the process-level settings — where the API listens,
how much of a body to keep, how long captures live. Its `targets` are only a
**seed**: they become the first project the first time a database is created,
and are ignored afterwards, so an edit made in the interface is never undone by
a stale file. Running with no configuration file at all is an ordinary first
run.

## Configuration

Copy `sonda.example.yaml` to `sonda.yaml` and add one entry per service.
Unknown keys are a startup error rather than a silent default, so a typo does
not turn into an hour of confusion.

```yaml
api_listen: 127.0.0.1:9000
database: sonda.db
max_body_bytes: 262144   # kept per body; the full body always reaches its destination
buffer_size: 1024        # captures buffered in memory before they are written

retention:
  max_calls: 50000
  max_age: 24h
  interval: 1m

targets:
  - name: admin-api
    listen: 127.0.0.1:9102
    upstream: http://127.0.0.1:3000
    protocol: http     # http, grpc, postgres or amqp

  - name: payments
    listen: 127.0.0.1:9103
    upstream: https://api.payments.example.com  # verified like any other client
    protocol: http
    tls: true                    # answer this port with a certificate, for callers that refuse http://
    insecure_skip_verify: false  # per service, never global. See TLS below

  - name: events
    listen: 127.0.0.1:9401
    upstream: amqp://127.0.0.1:5672  # amqps:// for a TLS broker
    protocol: amqp
```

Then point whatever calls `admin-api` at `127.0.0.1:9102`. The same binary and
the same file work for services in containers and for services running natively
— which is the point, since a real local stack is usually both.

Inside Docker, use `host.docker.internal` to reach a service running on the
host. See `sonda.docker.yaml`.

