# The test bed: a bookshop that half works

A small system for Sonda to watch, and a walkthrough that exercises every
capability against it.

Sonda's own `examples/echo` and `examples/grpcdemo` are enough to see that a
capture appears. They are not enough to see a request tree, a statement hanging
under the HTTP call that ran it, a contract that drifted, a gRPC failure hiding
under HTTP 200, or a credential that the web interface shows and the MCP server
does not. This is a system that produces all of it, continuously, with the
healthy traffic and the broken traffic in the field at the same time.

**The contrast is the point.** Roughly two thirds of what flows through here
works and one third fails, side by side, so a reader can see what a working call
looks like next to a broken one and tell them apart at a glance.

*[Léelo en español](README.es.md).*

---

## Contents

- [The shop](#the-shop)
- [Start it](#start-it)
- [What is flowing](#what-is-flowing)
- [The switches](#the-switches)
- [Before you start the exercises](#before-you-start-the-exercises)
- **The exercises**
  - [1. Is anything being captured](#1-is-anything-being-captured)
  - [2. The field: healthy and broken side by side](#2-the-field-healthy-and-broken-side-by-side)
  - [3. Search](#3-search)
  - [4. The failures a status code would miss](#4-the-failures-a-status-code-would-miss)
  - [5. The request tree](#5-the-request-tree)
  - [6. The same tree with a request id](#6-the-same-tree-with-a-request-id)
  - [7. PostgreSQL](#7-postgresql)
  - [8. Contract drift](#8-contract-drift)
  - [9. Diff](#9-diff)
  - [10. Replay](#10-replay)
  - [11. Stub mode](#11-stub-mode)
  - [12. Breaking things on purpose](#12-breaking-things-on-purpose)
  - [13. A service that is simply down](#13-a-service-that-is-simply-down)
  - [14. WebSocket and server-sent events](#14-websocket-and-server-sent-events)
  - [15. TLS](#15-tls)
  - [16. Why am I seeing nothing](#16-why-am-i-seeing-nothing)
  - [17. The MCP surface](#17-the-mcp-surface)
  - [18. Credentials do not leave over MCP](#18-credentials-do-not-leave-over-mcp)
  - [19. The terminal client](#19-the-terminal-client)
- [Stop it](#stop-it)
- [What this test bed does not cover](#what-this-test-bed-does-not-cover)

---

## The shop

Five services and a database. Every arrow in this diagram is a Sonda port: no
service knows the address of any other service, only the address of the proxy
in front of it.

```
                      ┌──────────────────────────────────────────┐
   you / driver ──▶ 9401 gateway                                  │
                      │                                           │
                      ├─▶ 9402 catalog ──▶ 9406 catalog-db ──▶ postgres
                      ├─▶ 9404 pricing        (gRPC, no reflection)
                      ├─▶ 9403 storefront     (GraphQL, WebSocket, SSE)
                      └─▶ 9405 shipping       (not running, on purpose)
                      └──────────────────────────────────────────┘

                            Sonda's own interface: 9400
```

| Port | Channel | What it is |
|---|---|---|
| `9400` | — | Sonda's web interface, query API and MCP endpoint |
| `9401` | `gateway` | HTTP. The front door, and the only service that calls other services |
| `9402` | `catalog` | HTTP. Books and members. The only service that runs SQL |
| `9403` | `storefront` | HTTP. GraphQL, a WebSocket and an event stream |
| `9404` | `pricing` | gRPC, **serving no reflection** |
| `9405` | `shipping` | HTTP. Declared, deliberately not running |
| `9406` | `catalog-db` | PostgreSQL |
| `9407` | `storefront-tls` | The same storefront behind a TLS listener |
| `8101` | — | The catalog's own port, straight past Sonda. Control endpoint only |
| `8102` | — | The storefront's own port, same |

Nothing here collides with the repository's own `compose.yaml`, which owns
`9000`, `9101` and `9201`. **Both can run at the same time**, which is worth
doing once just to see that two Sondas with two databases do not interfere.

The domain is a bookshop: `DUNE`, `PALE-FIRE`, `SOLARIS` are stocked;
`RESTRICTED-1` is in the reserve collection; `KAPUT` is a SKU that breaks the
catalog on purpose.

## Start it

```bash
cd testbed
docker compose up -d
```

Then open **http://127.0.0.1:9400**.

Within about twenty seconds the field has traffic in it, and about a third of
that traffic is red. Sonda opens filtered to faults, which is why the first
thing you see is the failures; `ALL` switches to everything.

To watch what the shop's customers are doing:

```bash
docker compose logs -f driver
```

## What is flowing

The `driver` service runs the same eighteen steps every twenty seconds. It is
deliberately the same eighteen: an exercise that says "look at the third one"
has to mean it. Nothing here is random.

```
── cycle 4 ──
   checkout DUNE                                                          ok
   checkout PALE-FIRE x5                                                  ok
   checkout RESTRICTED-1 (gRPC PermissionDenied under HTTP 200)           FAILED
   checkout SOLARIS for a customer that does not exist (GraphQL errors …) FAILED
   checkout DUNE express (shipping is not running)                        FAILED
   checkout KAPUT (an ordinary HTTP 500 from the catalog)                 FAILED
   report 400ms (slow, succeeds)                                          ok
   report 2500ms (the gateway gives up at 1s)                             FAILED
   reviews for DUNE                                                       ok
   reviews from a table that does not exist (SQL error, no status code)   FAILED
   search the catalog                                                     ok
   log in (password in a JSON body and in a SQL literal)                  ok
   oauth callback (credentials in the query string)                       ok
   websocket, closed cleanly                                              ok
   websocket, closed with 1011 and a reason                               FAILED
   server-sent events                                                     ok
   gRPC server stream                                                     ok
   graphql batch                                                          ok
```

Seven different kinds of failure are in there, and they are different on
purpose, because Sonda handles them differently and they look different:

| Kind | Where it comes from | What the transport says |
|---|---|---|
| An ordinary 5xx | `GET /books/KAPUT` | HTTP 500 |
| A gRPC status | `Quote` for a `RESTRICTED-*` SKU | **HTTP 200** |
| GraphQL errors | any operation for customer `ghost` | **HTTP 200** |
| A SQL error | `GET /reviews?broken=1` | **no status code at all** |
| A service that is down | any express checkout | HTTP 502 from Sonda, with the dial error |
| A timeout | `GET /report?ms=2500` | the caller stops waiting; no status |
| A socket closing badly | the WebSocket that is told `boom` | HTTP 101, close code 1011 — **and Sonda does not flag it** |

The three in bold are the ones worth demonstrating deliberately: **a tool that
reads the status code alone reports all three as successes.**

The last row is the honest exception, and [exercise 14](#14-websocket-and-server-sent-events)
comes back to it: the close code and its reason are in the capture, but a socket
that ends badly is not counted as a fault the way a gRPC status or a GraphQL
error is. The driver's log calls that step `FAILED` because the driver read the
close code; Sonda's fault filter does not.

And two things that are not failures but are worth telling apart from one:

- `GET /report?ms=400` is **slow and fine** — a wide mark in the field, not a
  fault bar. Latency is a different problem from failure.
- Every checkout that fails does so **as one branch of a request whose other
  branches worked**. That is the shape a real outage has.

## The switches

Failures that depend on the request need no switch and are always flowing. Two
things are state rather than a request, so they are switches — and they are
switches rather than probabilities for the same reason Sonda's own fault
injection counts calls instead of rolling dice: an exercise that says "now break
it and look again" has to give the same answer twice.

```bash
# the catalog's contract drifts: a field goes away, another changes type
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' \
  -d '{"drift":true}'

# the storefront answers GraphQL with a 502 HTML page instead of JSON
curl -X POST http://127.0.0.1:8102/_control -H 'Content-Type: application/json' \
  -d '{"graphql_down":true}'

# what is currently thrown
curl -s http://127.0.0.1:8101/_control
```

Those go to the service's **own** port, not through Sonda. Throwing a switch is
stage management, and a capture of it in the middle of the field is noise in the
exercise it was setting up.

## Before you start the exercises

**Stop the driver.** The exercises below make one call and then look at it; live
traffic in the background makes "the latest call" a moving target, and it also
puts unrelated calls inside the time window the request tree is built from.

```bash
docker compose stop driver     # and `docker compose start driver` when you are done
```

**These commands are Linux/macOS shell.** On Windows PowerShell 5.1, quotes
inside `-d '{"a":"b"}'` are rewritten before `curl.exe` sees them and the body
arrives mangled. Either use `Invoke-RestMethod`, or put the body in a file and
use `--data-binary '@body.json'`. The repository's own README has a section on
this.

**`jq` is optional.** Every command below works without it; it is only there to
make the JSON readable. Drop the `| jq …` and you get the same answer in one
line.

**Some exercises change state.** Each one says how to put it back. If you lose
track, `docker compose restart sonda` forgets every stub and every fault rule —
neither is ever written to the database — and the control switches go back to
off when their service restarts.

---

# The exercises

## 1. Is anything being captured

**Run**

```bash
curl -s http://127.0.0.1:9400/api/stats
```

**What a correct result looks like**

```json
{"calls":113,"oldest":"…","newest":"…","dropped":0,
 "by_target":[{"target":"catalog","calls":28,"faults":6,…}, …]}
```

Six or seven targets, a non-zero count on each, and `dropped: 0`.

**Worth knowing.** `dropped` is captures thrown away because the write buffer
was full. Capture never delays traffic — under a burst Sonda drops rather than
slowing down the system it is watching — so this number is how you find out that
it happened, instead of quietly getting an incomplete picture.

---

## 2. The field: healthy and broken side by side

**Look at** http://127.0.0.1:9400

**What a correct result looks like.** One lane per channel against a live time
axis. Start the driver again (`docker compose start driver`) and watch a cycle
arrive: `catalog` and `gateway` fill with marks, and every twenty seconds a
cluster of **full-height bars** appears — the failures. A fault is a different
*shape*, not just a different colour, which is what makes it survive a red
channel and a colour-blind reader.

Press `ALL` to see the successes too. The contrast is the thing: the same
`gateway` lane carries the two checkouts that worked and the four that did not,
in the same second.

Rest the pointer in the field and the trace **holds** so a mark stops sliding
while you aim at it.

**Worth knowing.** The channel rail's counts are unfiltered on purpose. The rail
answers "is this service healthy", so putting a filter in front of the field
must not change it.

Stop the driver again before continuing.

---

## 3. Search

**Run**

```bash
# by channel
curl -s 'http://127.0.0.1:9400/api/calls?target=pricing&limit=3'

# by text in the payloads, including payloads Sonda only has as bytes
curl -s 'http://127.0.0.1:9400/api/calls?q=RESTRICTED-1&limit=3'

# by path substring
curl -s 'http://127.0.0.1:9400/api/calls?path=/books/&limit=3'
```

**What a correct result looks like.** `q=RESTRICTED-1` finds the gateway
checkout, the catalog lookup **and the SQL statement underneath it** — three
channels and two protocols, matched on a string that was inside the bodies:

```
188 gateway     http      /checkout
186 catalog     http      /books/RESTRICTED-1
185 catalog-db  postgres  bookshop
```

**Worth knowing.** `q` is a literal phrase, not a query language, so
`q={"sku":"DUNE"}` works as typed. `path` is a substring match; `q` searches the
payloads as well — including the SQL, its bind parameters and the server's
complaint, not only the summary line.

The `pricing` call is **not** in that list even though the SKU was in its
request, because a protobuf message is binary and genuinely binary payloads are
not indexed. Search for a gRPC call by `target`, `path` or `grpc_status`
instead.

---

## 4. The failures a status code would miss

This is the exercise that justifies the tool.

**Run**

```bash
curl -s 'http://127.0.0.1:9400/api/calls?failed=true&limit=12'
```

**What a correct result looks like** — twelve rows in which the `status` column
alone would be lying about at least three of them:

```
209  catalog     http      GET  /reviews?sku=DUNE&broken=1   status=502  gql=0  pg=0
208  catalog-db  postgres  STATEMENT bookshop                status=0    gql=0  pg=1
205  gateway     http      GET  /report?ms=2500              status=504  gql=0  pg=0
204  catalog     http      GET  /slow?ms=2500                status=502  gql=0  pg=0   context canceled
201  gateway     http      POST /checkout                    status=502  gql=0  pg=0
200  catalog     http      GET  /books/KAPUT                 status=500  gql=0  pg=0
198  shipping    http      POST /quote                       status=502  gql=0  pg=0   dial tcp: lookup shipping…
192  storefront  http      POST /graphql                     status=200  gql=1  pg=0
187  pricing     grpc      POST /shop.v1.Pricing/Quote       status=200  gql=0  pg=0   grpc=7
```

Read the three that matter:

- **`187` is HTTP 200 and a failure.** gRPC reports below HTTP: the status is in
  the trailers, and it is `7 PermissionDenied`. Every gRPC tool that filters on
  HTTP status shows this call as healthy.
- **`192` is HTTP 200 and a failure.** GraphQL answers 200 with an `errors`
  array. Same problem, same answer: Sonda counts it as a fault in the filter,
  the rail, the field, the terminal, the tree and `recent_failures`.
- **`208` has no status code at all.** A SQL error is an `ErrorResponse` inside
  the stream. There is no transport-level signal anywhere.

**Now look at one of each.**

```bash
# the gRPC one: HTTP 200, gRPC 7, and the message percent-decoded
curl -s 'http://127.0.0.1:9400/api/calls?target=pricing&failed=true&limit=1'
```

```json
{"id":187,"target":"pricing","protocol":"grpc","method":"POST",
 "path":"/shop.v1.Pricing/Quote","status":200,
 "grpc_status":7,"grpc_status_text":"PermissionDenied",
 "grpc_message":"this title is held in the reserve collection — ask a librarian"}
```

`status` and `grpc_status` are both in the listing, side by side, because
showing one without the other is how this class of bug survives.

```bash
# the GraphQL one, in full
curl -s 'http://127.0.0.1:9400/api/calls?target=storefront&failed=true&limit=1'
```

```json
{"id":192,"status":200,"graphql_op":"query CustomerOrders","graphql_errors":1}
```

The operation is on the row. Without that, a GraphQL service arrives in the
field as one endpoint called two hundred times and the timeline stops being
useful for it. Open the call itself and the inspector has the operation, the
variables it was sent, and each error with its path and its `extensions.code`:

```bash
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=storefront&failed=true&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$ID" | jq .graphql
```

```json
{"batch": false,
 "operations": [{"type":"query","name":"CustomerOrders","label":"query CustomerOrders",
   "fields":["customer"], "variables":{"id":"ghost"},
   "errors":[{"message":"no customer with that id","path":"customer","code":"NOT_FOUND"}]}],
 "errors": 1}
```

**Worth knowing.** The GraphQL document is detected by the **body**, not the
path — a POST whose JSON carries a `query` string is a GraphQL request wherever
it was sent. And a batch is every operation in it: the driver sends one every
cycle, and it comes back labelled `batch of 2: query Shelf, query CustomerOrders`.

**"Zero errors" and "could not tell" are different answers.** Throw the
storefront's other switch and the service answers an HTML error page instead of
JSON:

```bash
curl -s -X POST http://127.0.0.1:8102/_control -H 'Content-Type: application/json' -d '{"graphql_down":true}'
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' -X POST http://127.0.0.1:9403/graphql \
  -H 'Content-Type: application/json' -d '{"query":"query Shelf { shelf }"}'
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=storefront&path=/graphql&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$ID" | jq .graphql
curl -s -X POST http://127.0.0.1:8102/_control -H 'Content-Type: application/json' -d '{"graphql_down":false}'
```

```
502 text/html; charset=utf-8
```

```json
{"batch":false,
 "operations":[{"type":"query","name":"Shelf","label":"query Shelf","fields":["shelf"]}],
 "errors":0,"unreadable":true}
```

The operation is still named — that came from the *request* — and the response
is marked **`unreadable`** rather than reported as a call with no errors. Sonda
reads enough of the document to label it and no more; it is not a parser, it
does not validate, and it does not know your schema. A response cut by the body
cap, or an error page from something sitting in front of the service, gets the
same honest answer.

---

## 5. The request tree

The single most valuable thing to be able to see, and the hardest to get from
logs.

**Run**

```bash
# one checkout that works
curl -s -X POST http://127.0.0.1:9401/checkout \
  -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":2,"customer":"cust-9"}' > /dev/null

# the id of that call
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

**What a correct result looks like**

```
(grouped by timing, not by a trace id — the shape is inferred)
gateway /checkout                                              4ms ✓
├─ catalog /books/DUNE                                         1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              1ms ✓
└─ storefront /graphql                                         0ms ✓
```

Four services and a database, in one request, arranged by who contained whom.
**The SQL statement is a child of the HTTP call that ran it** — which is the
whole reason a Postgres capture is one statement and not one connection: behind
a pool, one connection carries hours of an application's SQL, and hours of SQL
is not something you can hang off a request.

**Now the same request with a branch that failed:**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout \
  -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1","express":true}' > /dev/null

ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

```
gateway /checkout                                           1005ms ✗
├─ catalog /books/DUNE                                         1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              0ms ✓
├─ storefront /graphql                                         0ms ✓
└─ shipping /quote                                          1000ms ✗
       dial tcp: lookup shipping on 127.0.0.11:53: … i/o timeout
```

Three branches worked, one did not, and the failed one is the last thing the
request did. That is the reading a log cannot give you: the failure is not an
isolated red line, it is the fourth step of an operation whose first three were
fine — and the 1005 ms says the whole request was spent waiting for it.

Try the other failing checkouts and read the shape of each:

```bash
# fails at pricing, over gRPC, under HTTP 200
-d '{"sku":"RESTRICTED-1","quantity":1,"customer":"cust-1"}'
# fails at the storefront, over GraphQL, under HTTP 200
-d '{"sku":"SOLARIS","quantity":1,"customer":"ghost"}'
# fails at the catalog, with an ordinary 500
-d '{"sku":"KAPUT","quantity":1,"customer":"cust-1"}'
```

**Worth knowing.** The header line says the grouping was **inferred**. Nothing in
the shop sends a trace id, so Sonda arranged these by containment: a call that
begins no earlier and ends no later than another is its child. That is a guess,
and Sonda says so rather than presenting it as fact. It is also why the driver
runs its steps one at a time — two overlapping requests would make the nesting
genuinely ambiguous, and Sonda would honestly mark it `ambiguous`.

---

## 6. The same tree with a request id

**Run**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout \
  -H 'Content-Type: application/json' \
  -H 'X-Request-Id: shelf-audit-1' \
  -d '{"sku":"DUNE","quantity":2,"customer":"cust-9"}' > /dev/null

ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq '{certain: .trace.certain, trace_id: .trace.trace_id, calls: .trace.calls}'
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

**What a correct result looks like**

```json
{"certain": true, "trace_id": "shelf-audit-1", "calls": 4}
```

```
gateway /checkout                                              4ms ✓
├─ catalog /books/DUNE                                         1ms ✓
├─ pricing /shop.v1.Pricing/Quote                              1ms ✓
└─ storefront /graphql                                         0ms ✓
```

The gateway forwards the request id it was given — to the HTTP hops as a header
and to the gRPC hop as metadata, which is the same thing on HTTP/2 — so Sonda
groups by the id and the "inferred" warning is gone. `certain: true`.

**Worth knowing, and this is the subtlety of the exercise: the SQL statement is
no longer in the tree.** Four calls, not five. A trace id groups by *exactly*
that id, and a Postgres statement has nowhere to carry one — the protocol has no
headers. So propagating a trace id buys certainty for the hops that can carry it
and loses the hop that cannot.

Both readings are true and both are useful. Send an id when you want to know for
certain which calls belonged together; send none when you want the database
underneath them.

The gateway only ever forwards an id that arrived and never invents one, which
is what a real gateway does — and what lets this test bed show both.

---

## 7. PostgreSQL

**Run**

```bash
curl -s http://127.0.0.1:9402/reviews?sku=DUNE > /dev/null
curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=5' \
  | jq -r '.calls[] | "\(.id)  \(.method)  \(.path)  |  \(.postgres_summary)"'
```

**What a correct result looks like**

```
215  STATEMENT  bookshop  |  SELECT stars, body FROM reviews WHERE sku = $1 ORDER BY id -> SELECT 2
211  STATEMENT  bookshop  |  SELECT sku, title, author, price_cents, discount_pct, in_stock FROM books WHERE sku = $1 -> SELECT 1
208  STATEMENT  bookshop  |  error 42P01: relation "reviews_archive_2019" does not exist
```

One row per statement. The method is `STATEMENT`, the path is the database name
read off the startup message, there is no HTTP status and none is shown. The
summary carries the SQL and how it ended — the command tag and its row count, or
the error.

**A SQL error is a failure**, and `208` above proves it: `pg_errors: 1`, caught
by `?failed=true` with no status code anywhere in sight.

**Open a statement and read the exchange:**

```bash
SQL=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$SQL" | jq '.postgres.sent'
```

```json
[{"type":"P","kind":"parse","size":62,"from_client":true,
  "sql":"SELECT stars, body FROM reviews WHERE sku = $1 ORDER BY id"},
 {"type":"B","kind":"bind","size":18,"from_client":true,
  "params":[{"size":4,"text":"DUNE"}]},
 {"type":"D","kind":"describe","size":2,"from_client":true},
 {"type":"E","kind":"execute","size":5,"from_client":true},
 {"type":"S","kind":"sync","size":0,"from_client":true}]
```

The SQL and the value bound to `$1`, together, in the capture the text actually
crossed in.

### The password never reaches the capture

Log in. The catalog opens a connection of its own for this, so the startup
handshake is in the capture rather than only in the first statement the process
ever ran:

```bash
curl -s -X POST http://127.0.0.1:9402/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@bookshop.test","password":"shelf-of-books"}'

SQL=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$SQL" | jq '.postgres.sent, .postgres.received[:4]'
```

**What a correct result looks like**

```json
[{"kind":"startup","protocol":"3.0","parameters":{"database":"bookshop","user":"shop"}},
 {"type":"p","kind":"authentication_response","size":50,
  "note":"not decoded: this message carries a password or a SASL exchange"},
 {"type":"p","kind":"authentication_response","size":104, "note":"…"},
 {"type":"Q","kind":"query","sql":"SELECT name FROM members WHERE email = 'ada@bookshop.test' AND password = 'shelf-of-books'"}]
```

```json
[{"type":"R","kind":"authentication","auth":"sasl"},
 {"type":"R","kind":"authentication","auth":"sasl_continue"},
 {"type":"R","kind":"authentication","auth":"sasl_final"},
 {"type":"R","kind":"authentication","auth":"ok"}]
```

**The database password is gone and the fact of the authentication is not.** The
SASL exchange was blanked in the tap, as the bytes went past, before anything was
stored — replaced by filler of the same length, so every length field in the
stream is still true and the capture still reads back as a conversation. What
survives is that an authentication happened and by which mechanism.

This is the one place Sonda rewrites what it keeps, and the reason is that the
alternative cannot be undone: a live credential in a plaintext file. **What was
forwarded is untouched** — the real password reached PostgreSQL and the login
worked, which is why there is a row to look at.

**And notice what is *not* blanked here:** the member's password, sitting in the
SQL as a literal. That is application data crossing the wire, and the API shows
you everything, because here the reader is you. [Exercise 18](#18-credentials-do-not-leave-over-mcp)
is the same statement asked for over MCP, where the answer leaves the machine.

**Worth knowing.** The DSN in `compose.yaml` sets
`default_query_exec_mode=exec`. Left at its default, pgx caches statements under
a name and prepares them in an earlier cycle than the one that runs them; Sonda
only shows the SQL in the capture the text actually crossed in, so the
interesting row would read `1 bind, 1 execute, 1 sync` with no SQL on it. That
is a real Sonda limitation and this is the client-side knob for it.

---

## 8. Contract drift

**Run**

```bash
# a baseline exists already: the shop has been answering /books/DUNE all along
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":true}'
curl -s http://127.0.0.1:9402/books/DUNE

curl -s 'http://127.0.0.1:9400/api/drift?target=catalog&path=/books/DUNE' | jq -r .rendered
```

**What a correct result looks like**

```
+ cached   (boolean)
- discount_pct   (was number)
~ price_cents   number -> string
```

```bash
curl -s 'http://127.0.0.1:9400/api/drift?target=catalog&path=/books/DUNE' | jq '.baseline, .latest, .breaking'
```

```json
2
296
[{"path":"discount_pct","kind":"gone","was":"number"},
 {"path":"price_cents","kind":"retyped","was":"number","now":"string"}]
```

Three changes, two of which would break a caller. **Adding a field is safe;
losing one or changing its type is what takes a caller down**, and the report
says which is which rather than dumping all three as "changes".

**Put it back:**

```bash
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":false}'
```

**Worth knowing.**

- It compares **shapes, not values**. Two calls returning different prices are
  not drift. `price_cents: 1890` and `price_cents: "1890"` is.
- The baseline is `2` — one of the first captures Sonda ever took of this
  endpoint. Nobody wrote a schema and nobody maintains one; a baseline that
  needs maintaining is a baseline that is gone in three weeks.
- This is the one thing in Sonda that never touches the proxy. It only reads
  what was already stored.
- **Order matters here.** Drift needs the latest capture of the endpoint to be
  JSON. If you have just run [exercise 12](#12-breaking-things-on-purpose) and
  left a forced 503 on the catalog, the latest capture is a plain-text fault page
  and drift answers `422: call N did not answer JSON, so it has no shape to
  compare`. Clear the fault and make one more real call.

---

## 9. Diff

### The same endpoint, working and failing

```bash
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1"}' > /dev/null
OK=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"RESTRICTED-1","quantity":1,"customer":"cust-1"}' > /dev/null
BAD=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s "http://127.0.0.1:9400/api/diff?a=$OK&b=$BAD" | jq '.metadata, .request.changes, [.response.changes[].path]'
```

**What a correct result looks like**

```json
[{"path":"http_status","kind":"changed","a":200,"b":502}]
[{"path":"sku","kind":"changed","a":"DUNE","b":"RESTRICTED-1"}]
["book","customer","detail","error","quantity","quote","sku","status","step"]
```

"This one worked and this one did not — what changed?" answered in three lines:
the status went 200 → 502, **exactly one field of the request was different**,
and the response lost `book`, `customer` and `quote` and gained `error`, `step`
and `detail`.

The middle line is the one that earns the feature. Two requests that differ in a
single field, and a structural comparison that says which field — instead of a
wall of red and green in which reordered keys and reindented blocks look like
changes too.

### A replay against its original

See [exercise 10](#10-replay).

### Diff and drift are different questions

Run the drift switch on and diff two captures of `/books/DUNE`:

```bash
curl -s http://127.0.0.1:9402/books/DUNE > /dev/null
A=$(curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&path=/books/DUNE&limit=1' | jq '.calls[0].id')
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":true}'
curl -s http://127.0.0.1:9402/books/DUNE > /dev/null
B=$(curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&path=/books/DUNE&limit=1' | jq '.calls[0].id')

curl -s "http://127.0.0.1:9400/api/diff?a=$A&b=$B" | jq '.response.changes'
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":false}'
```

```json
[{"path":"cached","kind":"added","b":false},
 {"path":"discount_pct","kind":"removed","a":10}]
```

**Two changes, not three.** Drift reported `price_cents` retyped from number to
string; the diff does not, because `1890` and `"1890"` are **the same value**.
protojson renders int64 as a string, and reporting that as a difference would be
a statement about encoding rather than about data.

That is not an inconsistency: they answer different questions. Diff asks "is this
the same answer", drift asks "is this the same shape". The one pair of captures
demonstrates both.

**Worth knowing.** The diff is structural, so reordered keys and reindented
blocks are not differences. Array order *is* one — position carries meaning in a
protobuf repeated field. Duration is deliberately excluded: it changes on every
replay and would bury the differences that mean something.

---

## 10. Replay

**Run**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1"}' > /dev/null
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s -X POST "http://127.0.0.1:9400/api/calls/$ID/replay"
```

**What a correct result looks like**

```json
{"sent_to":"gateway","status":200,"duration_ms":3.64}
```

```bash
NEW=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$NEW" | jq '{id, replay_of, status}'
curl -s "http://127.0.0.1:9400/api/diff?a=$ID&b=$NEW" | jq '.response.identical'
```

```json
{"id": 113, "replay_of": 104, "status": 200}
true
```

The request went back out built from the bytes that were stored, so what reached
the gateway is what reached it the first time. It was sent **through Sonda**, not
straight at the service, so the replay is captured like any other traffic, is
linked to the call it came from, and the two can be compared immediately.

**Now try to replay something that cannot be:**

```bash
PG=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
curl -s -X POST "http://127.0.0.1:9400/api/calls/$PG/replay"
```

```json
{"error":"a postgres capture cannot be replayed: it belongs to a connection that
is gone, and sending it again would open a new one rather than repeat this one"}
```

**Worth knowing.** HTTP 409, and the refusal says why. A WebSocket is refused for
the same reason. So is a truncated capture: only the head of the body was
stored, so what went out would not be what was captured, and the result would
carry the word "replay" while being a different request. Every surface refuses,
including the API itself, so an agent gets the same answer the browser does.

Replay only ever targets a **configured channel** — there is no arbitrary-URL
mode. The useful case is asking the same request of a second instance you are
already observing, and anything wider turns a debugger into a request forge.

---

## 11. Stub mode

**Run**

```bash
curl -s -X POST http://127.0.0.1:9400/api/stub -H 'Content-Type: application/json' \
  -d '{"service":"catalog","enabled":true}'

curl -s -D - -o /dev/null http://127.0.0.1:9402/books/DUNE | grep -iE 'HTTP|x-sonda'
```

**What a correct result looks like**

```
HTTP/1.1 200 OK
X-Sonda-Stub: 115
```

The catalog was never reached. That answer is the bytes of capture `115`,
replayed, and the header names which one so a recorded answer can never be
mistaken for a live one.

**Ask for something it has no recording of:**

```bash
curl -s -D - http://127.0.0.1:9402/books/NEVER-SEEN | grep -iE 'HTTP|x-sonda|Sonda is answering'
```

```
HTTP/1.1 501 Not Implemented
X-Sonda-Stub: none
Sonda is answering for "catalog" from recordings, and it has no recording of
GET /books/NEVER-SEEN. Make the call once with stubbing off, or turn it off for
this service.
```

A 501 that explains itself, not an invented answer and not a silent empty 200.

**gRPC works too**, which is the interesting half — the recorded trailers are
replayed, so the client gets a real `grpc-status` instead of waiting for one that
never comes:

```bash
curl -s -X POST http://127.0.0.1:9400/api/stub -H 'Content-Type: application/json' \
  -d '{"service":"pricing","enabled":true}'
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1"}' | jq .quote
```

The checkout still succeeds with the `pricing` container never touched. Look at
the capture and it carries `stub_of`, and the tree marks the branch
`[from recording]`.

**Put it back:**

```bash
curl -s -X POST http://127.0.0.1:9400/api/stub -H 'Content-Type: application/json' -d '{"clear":true}'
```

**Worth knowing.** An identical request body wins outright — that is the
difference between replaying *the answer to Quote* and *the answer to
Quote(DUNE)*. Failing that, the most recent call to the same method and path,
which for gRPC means the same RPC regardless of arguments. Captures that were
themselves stubbed are never reused: without that, leaving stubbing on would
slowly feed Sonda its own answers. And stubbing is **forgotten when Sonda
restarts**, deliberately — a stub that outlives a restart is one nobody
remembers turning on.

---

## 12. Breaking things on purpose

### A forced status, one call in three

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","status":503,"one_in":3}'

for i in 1 2 3 4 5 6; do
  printf "call %d -> %s\n" $i "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9402/books/DUNE)"
done
```

**What a correct result looks like**

```
call 1 -> 200
call 2 -> 200
call 3 -> 503
call 4 -> 200
call 5 -> 200
call 6 -> 503
```

**Deterministic, not random.** One call in every three, in that order, every
run. A percentage would behave differently each time and turn a failing test into
a coin toss. Note that it is the **third** call that breaks, not the first: the
counter counts every call to the service and fires when it divides. Changing the
rule restarts the schedule.

### It can never pass for a real failure

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","status":503,"latency_ms":300}'
curl -s -D - http://127.0.0.1:9402/books/DUNE | grep -iE 'HTTP|x-sonda|by Sonda'
```

```
HTTP/1.1 503 Service Unavailable
X-Sonda-Fault: answered 503 by Sonda on purpose, after 300ms of injected delay
answered 503 by Sonda on purpose, after 300ms of injected delay
```

The header, the body and the capture's `injected: true` all say the same thing.
A rule in force is also stated where the failures are being *read* — the channel
shows `BROKEN` and the readout at the top of the interface counts what is armed —
which matters most above `one_in: 1`, where the injected calls on their own look
like a service that is merely flaky.

### A cut connection

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","cut":true}'
curl -s http://127.0.0.1:9402/books/DUNE          # curl exits 52, empty reply
curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&limit=1' \
  | jq '.calls[0] | {id, status, injected, error}'
```

```json
{"id":154,"status":0,"injected":true,"error":"connection cut by Sonda on purpose"}
```

**A status or a cut ends the call at Sonda** — the service is never reached.

### Latency lets the call through

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","latency_ms":1500}'
curl -s -o /dev/null -w 'took %{time_total}s\n' http://127.0.0.1:9402/books/DUNE
curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&limit=1' \
  | jq '.calls[0] | {status, duration_ms, injected}'
```

```json
{"status":200,"duration_ms":1502.985,"injected":null}
```

**A delay-only fault is not marked `injected`, and this is worth knowing.** The
service really was called and really did answer 200; the only thing that is not
real is how long it took. There was no injected *failure* to record — which is
exactly the case a timeout is meant to catch, and the field draws it as a wide
mark rather than a fault bar.

**Put it back:**

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' -d '{"clear_all":true}'
```

Rules are forgotten when Sonda restarts, for the same reason stubbing is.

---

## 13. A service that is simply down

`shipping` is declared in Sonda's configuration and has no container running.
That is a different failure from a service that answered badly, and it should
look different.

**Run**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1","express":true}' | jq .step
curl -s 'http://127.0.0.1:9400/api/calls?target=shipping&limit=1' \
  | jq '.calls[0] | {status, error}'
```

**What a correct result looks like**

```json
"shipping"
{"status":502,"error":"dial tcp: lookup shipping on 127.0.0.11:53: … i/o timeout"}
```

**An unreachable upstream is captured too.** Sonda answers 502 and records the
transport error, so the failure is in the timeline rather than missing from it. A
502 that came from the upstream *itself* would have an empty `error` field —
that is how you tell "the service said 502" from "the service was not there".

**Now bring it up and change nothing else:**

```bash
docker compose --profile shipping up -d shipping
sleep 3
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1","express":true}' > /dev/null
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

```
gateway /checkout                                              7ms ✓
├─ catalog /books/DUNE                                         1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              1ms ✓
├─ storefront /graphql                                         1ms ✓
└─ shipping /quote                                             1ms ✓
```

The same request, six calls, all green, and 1005 ms became 4 ms. Put it back
with `docker compose stop shipping`.

**Worth knowing.** A container behind a profile is not removed by a plain
`docker compose down`, so if you tear the stack down and bring it back up, the
old `shipping` container is still there holding a reference to a network that no
longer exists, and starting it fails with `network … not found`. `docker compose
rm -sf shipping` clears it, and `docker compose down -v --profile shipping`
avoids it in the first place.

---

## 14. WebSocket and server-sent events

### A socket that closes badly

The driver holds two conversations every cycle: one that ends cleanly and one
that tells the storefront `boom`.

```bash
docker compose start driver && sleep 25 && docker compose stop driver

curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=2' \
  | jq -r '.calls[] | "\(.id)  status=\(.status)  \(.duration_ms)ms"'

WS=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$WS" | jq .socket
```

**What a correct result looks like**

```json
{"sent":[{"kind":"text","final":true,"size":4,"text":"boom"}],
 "received":[
   {"kind":"text","final":true,"size":42,"text":"{\"event\":\"welcome\",\"shelf\":\"new arrivals\"}"},
   {"kind":"close","final":true,"size":36,
    "close_code":1011,"close_reason":"inventory feed lost, no shelf data"}],
 "sent_summary":"1 text","received_summary":"1 text, 1 close",
 "sent_incomplete":false,"received_incomplete":false}
```

**The close frame reports its code and its reason, and that is usually the
answer to why the socket stopped working.** Compare with the other capture,
which closes 1000.

Note the client's frame reads as `boom` in plain text: client frames are
**unmasked for display**, because the mask is transport scaffolding the
receiving end removes before anything reads the payload. The key is kept so the
frame can still be reproduced exactly.

**Worth knowing.** The status is `101` — the handshake succeeded — and **the
capture only appears when the socket closes**, not while it is open. A socket is
one exchange and the exchange is not over. The handshake itself goes through
verbatim: the key, the subprotocols and the negotiated extensions are what the
two ends are agreeing on, and changing any of them would make the recorded
conversation a different one.

**And the part worth knowing most:** neither of those two captures is counted as
a failure.

```bash
curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&failed=true&limit=5' | jq '.calls | length'
curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=2' \
  | jq -r '.calls[] | "\(.id)  status=\(.status)  error=\(.error)"'
```

```
0
87  status=101  error=null
86  status=101  error=null
```

The socket that died with `1011 inventory feed lost` and the socket that said
goodbye are, to the fault filter, the same row. Sonda promotes a gRPC status, a
GraphQL `errors` array and a Postgres `ErrorResponse` into failures — three
protocols that report trouble below the transport — and a WebSocket close code is
a fourth case of exactly that shape which it does not promote. The information is
all there in `socket.received[].close_code`; nothing above 1000 reaches the fault
filter, the channel rail's counts or `recent_failures`.

So this is the one failure in the test bed you have to go and look for:

```bash
curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=10' \
  | jq -r '.calls[].id' \
  | while read id; do
      curl -s "http://127.0.0.1:9400/api/calls/$id" \
        | jq -r --arg id "$id" '.socket.received[] | select(.close_code) | "\($id)  \(.close_code)  \(.close_reason)"'
    done
```

```
87  1011  inventory feed lost, no shelf data
86  1000  goodbye
```

### An event stream

```bash
curl -s http://127.0.0.1:9403/events > /dev/null
SSE=$(curl -s 'http://127.0.0.1:9400/api/calls?path=/events&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$SSE" | jq '{protocol, status, stream}'
```

```json
{"protocol":"http","status":200,
 "stream":{"events":[
   {"name":"shelf","id":"1","data":"{\"sku\":\"DUNE\",\"on_hand\":4}"},
   {"name":"shelf","id":"2","data":"{\"sku\":\"DUNE\",\"on_hand\":3}"},
   {"name":"shelf","id":"3","data":"{\"sku\":\"DUNE\",\"on_hand\":2}"},
   {"name":"done","data":"{\"sent\":3}"}],
  "incomplete":false}}
```

**Worth knowing.** An event stream is **not a protocol of its own** — the
response is ordinary HTTP, `protocol: "http"`, and Sonda recognises it by the
response's content type and splits it back into its events for display. The
storefront sends a `: keep-alive` comment first and it is **not** in the list:
comments and keepalives are dropped rather than shown as events. `?broken=1`
ends the stream with an `error` event instead of `done`.

---

## 15. TLS

`storefront-tls` is the same storefront behind a listener that answers with a
certificate of its own, for callers that refuse to speak `http://`.

**Run**

```bash
curl -s http://127.0.0.1:9400/api/tls | jq '{exists, subject: .instructions.subject, path: .instructions.path}'
curl -s http://127.0.0.1:9400/api/tls/ca.pem -o sonda-ca.pem
```

```json
{"exists": true,
 "subject": "Sonda local CA (6bc4a74c91a4)",
 "path": "/data/sonda-ca.pem"}
```

The authority was created the first time a `tls: true` service started — not on
the first run. A Sonda with no TLS target never creates one and prints nothing.
The `path` is inside the container, which is why there is a download endpoint.

**Now call it:**

```bash
# fails: nothing on this machine trusts that authority, and Sonda installs nothing
curl -s https://localhost:9407/health

# works
curl -s --cacert sonda-ca.pem https://localhost:9407/health
```

```json
{"status":"ok"}
```

```bash
curl -s --cacert sonda-ca.pem -X POST https://localhost:9407/graphql \
  -H 'Content-Type: application/json' -d '{"query":"query Shelf { shelf }"}'
curl -s 'http://127.0.0.1:9400/api/calls?target=storefront-tls&limit=1' \
  | jq '.calls[0] | {id, status, tls, graphql_op}'
```

```json
{"id":158,"status":200,"tls":true,"graphql_op":"query Shelf"}
```

`tls: true` on the capture: a response read months later still says whether the
connection to Sonda was encrypted. The upstream hop is plain HTTP, so there is
no `upstream_tls` — the two halves are recorded separately, because they are two
different facts.

**Two things that will bite you, both of them the client's doing, not Sonda's:**

- **Use `localhost`, not `127.0.0.1`.** A client connecting to an IP literal
  sends no SNI, so Sonda mints the certificate for the address the connection
  arrived on — which, inside a container, is the container's own address on the
  compose network, not `127.0.0.1`. Verification then fails with *"certificate is
  not valid for 127.0.0.1"*. A hostname sends SNI and the certificate is minted
  for the name asked for.
- **On Windows**, `curl.exe` uses schannel, which cannot check revocation for a
  private authority and fails with `CERT_TRUST_REVOCATION_STATUS_UNKNOWN` even
  when `--cacert` is right. Add `--ssl-no-revoke`. macOS and Linux need nothing
  extra.

**Worth knowing.** Sonda **does not install anything**. It writes the two files
beside the database, prints what to run, and stops; `curl -s
http://127.0.0.1:9400/api/tls | jq .instructions` has the exact line for every
platform and the exact line to take it back out. Modifying a machine's trust
store is a decision with a person's hand on it — a debugging tool that did it
quietly would be indistinguishable from malware.

---

## 16. Why am I seeing nothing

The report that answers the most common first hour with a tool like this.

**Run**

```bash
# reads only what Sonda already knows, touches no network
curl -s http://127.0.0.1:9400/api/diagnose | jq '{verdict, summary}'

# additionally dials each upstream once and hangs up
curl -s -X POST http://127.0.0.1:9400/api/diagnose \
  | jq -r '.services[] | "\(.service)  \(.verdict)  connections=\(.connections) captures=\(.captures) reachable=\(.upstream_reachable)"'
```

**What a correct result looks like**

```
gateway         capturing  connections=7  captures=24  reachable=true
catalog         capturing  connections=30 captures=56  reachable=true
storefront      capturing  connections=6  captures=21  reachable=true
pricing         capturing  connections=3  captures=18  reachable=true
shipping        capturing  connections=2  captures=2   reachable=null
catalog-db      capturing  connections=3  captures=36  reachable=true
storefront-tls  capturing  connections=9  captures=2   reachable=true
```

**Worth knowing.**

- **Probing is a side effect**, which is why it is on the `POST` and not the
  `GET`. Finding out whether a service is up means dialling it, and that is
  traffic you did not send. It never happens on a page load, a refresh or a
  timer. The dial goes straight to the service, never through Sonda's own
  listener, so it can never turn up in the capture list looking like a call you
  made.
- **`connections` is the reading that does the most work.** It counts every TCP
  connection the port accepted, whether or not it became a call. Look at
  `storefront-tls`: if you ran [exercise 15](#15-tls) and made the call once
  before downloading the CA, its connection count is higher than its capture
  count, and the gap is exactly the handshakes that failed. Connections without
  captures is a client that found Sonda and was misunderstood — TLS against a
  plaintext listener or the reverse, or a protocol Sonda does not proxy. Zero
  connections is a client that never arrived. Those are different problems with
  different fixes, and without that count they read exactly the same.
- **Sonda cannot see a client that never connected to it.** A port with no
  connections reads the same way whether the caller is still talking to the
  service directly, is pointed at the wrong port, or has not run yet. The report
  names all three rather than picking one.
- **A verdict is the worst thing that is true, and `capturing` outranks a failed
  probe.** Stop a service and probe:

  ```bash
  docker compose stop catalog
  curl -s -X POST http://127.0.0.1:9400/api/diagnose \
    | jq -r '.services[] | select(.service=="catalog") | .verdict, .detail'
  docker compose start catalog
  ```

  ```
  capturing
  73 call(s) captured here, 18 flagged, the newest 19s ago. Traffic is reaching
  Sonda. The upstream http://catalog:8101 also refused a connection (dial tcp:
  lookup catalog …).
  ```

  Still `capturing`, and it is the right answer: traffic really is arriving at
  this port, and the proxy really is recording it. The dead upstream is in the
  detail rather than in the verdict. `upstream_unreachable` is what a service
  with **no** captures gets — a port nothing has ever come through, which is the
  case where the failed dial is the whole story.

---

## 17. The MCP surface

Sonda speaks the Model Context Protocol, so a coding agent reads the captures
itself instead of being told about them. It is a plain JSON-RPC POST — **no
handshake, no session id, no SSE** — so every one of these is pasteable.

**List the tools**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq -r '.result.tools[].name'
```

**What a correct result looks like** — twenty of them:

```
recent_failures  search_calls  get_call  trace_call  contract_drift  diff_calls
list_services  schema_status  diagnose_silence  trust_certificate  wait_for_call
replay_call  connect_project  configure_service  remove_service  upload_schemas
activate_project  set_stub  break_service  disconnect_project
```

**"What just broke?"**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"recent_failures","arguments":{"limit":5}}}' \
  | jq -r '.result.content[0].text'
```

**The whole request a call belonged to**

```bash
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",
       \"params\":{\"name\":\"trace_call\",\"arguments\":{\"id\":$ID}}}" \
  | jq -r '.result.content[0].text | fromjson | .rendered'
```

```
(grouped by timing, not by a trace id — the shape is inferred)
gateway /checkout                                              3ms ✓
├─ catalog /books/SOLARIS                                      1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              0ms ✓  [from recording]
└─ storefront /graphql                                         0ms ✓
```

**What is being watched, and what is stubbed or broken right now**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call",
       "params":{"name":"list_services","arguments":{}}}' \
  | jq -r '.result.content[0].text'
```

Every service comes back with the exact line to point a caller at it —
`CATALOG_URL=0.0.0.0:9402` — because **Sonda cannot repoint a caller**. It is an
explicit proxy and it sees nothing until something is told to talk to it. Sonda
knows the mapping; the agent has the filesystem and can restart a process.

The answer also carries what is stubbed and what is being broken **right now**,
which an agent reading captures needs before it draws a conclusion from them.
Arm both and ask again:

```json
{"stubbed": ["pricing"],
 "faults": {"catalog": "HTTP 503, one call in 10"},
 "projects": [ … ]}
```

**Where the gRPC field names came from**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":5,"method":"tools/call",
       "params":{"name":"schema_status","arguments":{}}}' \
  | jq -r '.result.content[0].text'
```

```json
{"schemas":[{"target":"pricing","source":"descriptor_set","reflection":true}]}
```

**`source: descriptor_set` is the point of this exercise.** The `pricing`
service serves no reflection — most services in the wild do not — so Sonda could
not ask it what its schema was. It read the compiled descriptor set mounted at
`/etc/bookshop/descriptors.binpb` instead, which is why
`{"sku":"RESTRICTED-1","quantity":1}` comes back with field names rather than as
`{"number":1,"type":"string","value":"RESTRICTED-1"}`.

To see what the third fallback looks like, delete the `descriptor_set:` line from
`sonda.testbed.yaml`, run `docker compose down -v && docker compose up -d`, wait
for a cycle, and read a `Quote` capture:

```json
{"schemas":[{"target":"pricing","source":"","reflection":true,
             "error":"no schema source configured"}]}
```

```json
{"index":0,"size":16,
 "fields":[{"number":1,"type":"string","value":"SOLARIS"},
           {"number":2,"type":"varint","value":2,
            "note":"could be an integer, a bool or an enum"},
           {"number":3,"type":"string","value":"USD"}]}
```

The message still decodes — nested structure intact, types read off the wire —
and **the guesses are labelled as guesses**. On the wire a varint really could be
an int32, a bool or an enum, and saying so is the difference between a useful
view and a misleading one. Put the line back and `down -v && up -d` again.

*(The `reflection: true` in that answer is a reporting bug in Sonda — the flag
is not read back from the stored service. `source` is the real answer.)*

**Wait for traffic that has not happened yet**

This is the tool that turns Sonda into a check rather than a viewer: the agent
makes a change, triggers the action, and waits for what should have gone over
the wire.

```bash
# in one terminal
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":6,"method":"tools/call",
       "params":{"name":"wait_for_call",
                 "arguments":{"service":"catalog","path":"/books/SOLARIS","timeout_seconds":15}}}' \
  | jq -r '.result.content[0].text | fromjson | {matched, waited_seconds}'

# in another, a couple of seconds later
curl -s http://127.0.0.1:9402/books/SOLARIS > /dev/null
```

```json
{"matched": true, "waited_seconds": 2}
```

Nothing arriving is also an answer: let it time out and it comes back
`{"matched": false, "hint": "…run diagnose_silence…"}`.

**Break a service by asking**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":7,"method":"tools/call",
       "params":{"name":"break_service","arguments":{"service":"storefront","latency_ms":1200,"one_in":2}}}' \
  | jq -r '.result.content[0].text'

for i in 1 2; do
  curl -s -o /dev/null -w "call $i took %{time_total}s\n" -X POST http://127.0.0.1:9403/graphql \
    -H 'Content-Type: application/json' -d '{"query":"query Shelf { shelf }"}'
done

curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":8,"method":"tools/call",
       "params":{"name":"break_service","arguments":{"clear_all":true}}}' > /dev/null
```

```
call 1 took 0.003453s
call 2 took 1.218154s
```

**Worth knowing.** `set_stub`, `break_service`, `replay_call`,
`activate_project` and `disconnect_project` are marked destructive, so a real MCP
client asks the human before running them. Raw JSON-RPC does not ask, because
there is nobody to ask.

Also on purpose: **there is no tool to delete a project**, no live stream, no way
to download the certificate authority's private key, and **no setting that turns
redaction off** — which is the next exercise.

---

## 18. Credentials do not leave over MCP

The web interface shows the stored application payloads used here, because
there the reader is you. MCP answers leave the machine, so they are filtered.
Postgres and AMQP handshake secrets are absent from both because Sonda blanks
them before persistence. The test bed puts a credential in each of the three
places that MCP filtering has to reach.

**Set it up**

```bash
curl -s -X POST http://127.0.0.1:9402/login \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer tok-live-2f8c11' \
  -d '{"email":"ada@bookshop.test","password":"shelf-of-books"}' > /dev/null

curl -s "http://127.0.0.1:9401/oauth/callback?code=ac-9f21&access_token=tok-live-2f8c11&state=shelf" > /dev/null

LOGIN=$(curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&path=/login&limit=1' | jq '.calls[0].id')
SQL=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
```

### A header and a JSON body

```bash
# the API: everything
curl -s "http://127.0.0.1:9400/api/calls/$LOGIN" | jq '{auth: .request.headers.Authorization, body: .request.text}'

# MCP: the same call
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"tools/call\",
       \"params\":{\"name\":\"get_call\",\"arguments\":{\"id\":$LOGIN,\"detail\":true}}}" \
  | jq -r '.result.content[0].text | fromjson | {auth: .request.headers.Authorization, body: .request.text}'
```

```json
{"auth": ["Bearer tok-live-2f8c11"],
 "body": "{\"email\":\"ada@bookshop.test\",\"password\":\"shelf-of-books\"}"}
```

```json
{"auth": ["[redacted by Sonda]"],
 "body": "{\"email\":\"ada@bookshop.test\",\"password\":\"[redacted by Sonda]\"}"}
```

The header is gone by name; the body key is gone by name, **inside a JSON body
that Sonda is storing as opaque bytes**. The email stays, because it is not a
credential.

### A query string

```bash
curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&path=/oauth/callback&limit=1' | jq -r '.calls[0].path'

curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":10,"method":"tools/call",
       "params":{"name":"search_calls","arguments":{"path":"/oauth/callback","limit":1}}}' \
  | jq -r '.result.content[0].text | fromjson | .calls[0].path'
```

```
/oauth/callback?code=ac-9f21&access_token=tok-live-2f8c11&state=shelf
/oauth/callback?code=[redacted by Sonda]&access_token=[redacted by Sonda]&state=shelf
```

Matching a field name only works on a field that has one, and a URL has no
fields. So query strings get their own pass, wherever a URL turns up — the
captured path, a `Location` redirect, a link inside a body. **The rest of the URL
is kept**, including `state`, because the path is how you recognise the call.

### A SQL statement

This is the hardest one, and the reason is that Postgres is column oriented: the
sensitive name and the sensitive value arrive in different messages.

```bash
curl -s "http://127.0.0.1:9400/api/calls/$SQL" | jq -r .postgres_summary

curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":11,\"method\":\"tools/call\",
       \"params\":{\"name\":\"get_call\",\"arguments\":{\"id\":$SQL,\"detail\":true}}}" \
  | jq -r '.result.content[0].text | fromjson | .postgres_summary'
```

```
SELECT name FROM members WHERE email = 'ada@bookshop.test' AND password = 'shelf-of-books' -> SELECT 1
SELECT name FROM members WHERE email = '[redacted by Sonda]' AND password = '[redacted by Sonda]' -> SELECT 1
```

**A statement that names a credential comes back with its structure intact and
its literals blanked.** All of them, including the email — over-blanking on
purpose, because knowing which literal binds to which column is a job for a SQL
parser and guessing wrong here is unrecoverable. You can still see what the query
did, which is what you came for.

And this happens in the **one-line summary a listing shows before you have asked
for anything**, not only in the detail. A credential that is only redacted once
you open the call is a credential that already went out in the search results.

**Worth knowing.** There is no setting to turn any of this off, deliberately: a
flag for it would be switched on against a toy project and then forgotten against
a real one. `SECURITY.md` lists what redaction reaches and, more usefully, what
it does not — a protobuf field decoded without a schema has a number and no name,
so there is nothing for name matching to match.

---

## 19. The terminal client

The same instrument in a terminal, and a second client of the same API rather
than a second implementation: it captures nothing and stores nothing.

```bash
docker compose run --rm tui
```

**What a correct result looks like.** The channel rail with the shop's six
channels, their call counts and their fault counts; the field, with a **full
block where an ordinary call is a half one** — a lane is one row tall, so a fault
cannot be a taller bar and becomes a different glyph instead. Shape still carries
the outcome before colour does.

Try, in order:

| Key | What to look for |
|---|---|
| `↑` `↓` | pick the `gateway` channel |
| `←` `→` | step along it, call by call — not cell by cell, an empty cell is not something to point at |
| `enter` | read the selected call |
| `t` | the whole request as a tree, the same one exercise 5 produced |
| `c` | contract drift for this endpoint |
| `d` | diff a replay against its original |
| `f` | faults only, or everything |
| `/` | search |
| `q` | quit |

Arm a fault (`break_service` or `POST /api/faults`) and watch the channel: the
fault block is engraved before its name and the bar counts how many are armed. A
service being broken on purpose is a **mode** and not a call, so it belongs on
the channel rather than in the field.

---

## Stop it

```bash
docker compose --profile shipping --profile tui down      # keeps the captures
docker compose --profile shipping --profile tui down -v   # throws the database away too
```

Name the profiles. A plain `docker compose down` leaves any container you started
behind a profile in place, pointing at a network that has just been deleted, and
the next `up` for it fails with `network … not found`.

The volume is worth keeping between sessions: contract drift compares against the
oldest capture Sonda holds, and starting clean throws that baseline away.

## What this test bed does not cover

Stated because a walkthrough that quietly skips something is worse than one that
says what it skipped.

- **AMQP.** Sonda captures AMQP 0-9-1, but this test bed does not include a
  RabbitMQ broker or an AMQP exercise. The focused proxy tests use a deterministic
  wire-level broker instead of presenting that as RabbitMQ compatibility proof.
- **Kafka.** Deliberately absent from Sonda; the repository's README explains
  why, and it is not about the protocol.
- **An upstream that speaks TLS.** `storefront-tls` exercises the half where the
  *client* speaks TLS. The other half — Sonda verifying a real certificate on the
  way out, and `insecure_skip_verify` for a self-signed one — needs a certificate
  authority the whole system agrees on, which is a bigger apparatus than a test
  bed should carry. The configuration is two lines in
  `sonda.example.yaml` if you want to point one at a real HTTPS API.
- **Client streaming gRPC.** Unary and server streaming are here; the third
  shape is in `examples/grpcdemo` in the main repository.
- **Truncation and body caps.** `max_body_bytes` is at its default and nothing
  here sends a payload near it. Lower it to `512` in `sonda.testbed.yaml` and
  every capture starts reporting `truncated`, and replay starts refusing.
- **A compressed gRPC message.** Reported as compressed and not decoded;
  nothing here negotiates compression.
- **Project switching and importing.** One project, seeded from
  `sonda.testbed.yaml`. `POST /api/discover` against this directory's
  `compose.yaml` is a reasonable thing to try, and the `PROJECTS` screen is
  where the rest of it lives.

---

## How it is built

`testbed/` is **its own Go module**. Sonda is a single static binary with no
cgo, which is why six platforms cross-compile from one runner and why the image
is 50 MB; a PostgreSQL client in the main `go.mod` would end that. Nothing in
here changes the main module's `go.mod` or `go.sum`, and `go build ./...` at the
repository root does not descend into it.

```
testbed/
  compose.yaml            the shop, and a Sonda watching it
  sonda.testbed.yaml      Sonda's seven targets
  descriptors.binpb       the pricing schema, for a service with no reflection
  db/init.sql             five books, four reviews, two members
  cmd/gateway             the fan-out
  cmd/catalog             HTTP + PostgreSQL
  cmd/storefront          GraphQL + WebSocket + SSE
  cmd/pricing             gRPC, no reflection
  cmd/shipping            not running, on purpose
  cmd/driver              eighteen steps, every twenty seconds
  internal/toy            JSON replies, the control switches, trace propagation
  internal/ws             a WebSocket small enough to read
```

After changing `proto/shop/v1/pricing.proto`:

```bash
cd testbed
buf lint && buf generate && buf build -o descriptors.binpb
```
