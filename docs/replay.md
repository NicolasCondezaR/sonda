[← Docs](README.md)

# Replay and diff

Selecting a call in the inspector offers **REPLAY**. The request goes back out
built from the bytes that were stored, so what reaches the service is what
reached it the first time.

It is sent **through Sonda**, not straight at the upstream, which means the
replay is captured like any other traffic: it lands in the field, it is linked
to the call it came from, and the two can be compared immediately.

```bash
# replay a call onto the channel it came from
curl -X POST http://127.0.0.1:9000/api/calls/42/replay

# or onto another configured channel, to ask the same request of a second instance
curl -X POST http://127.0.0.1:9000/api/calls/42/replay \
  -H 'Content-Type: application/json' -d '{"target":"orders-staging"}'

curl 'http://127.0.0.1:9000/api/diff?a=42&b=43'
```

Replay only ever targets a **configured channel**. There is no arbitrary-URL
mode: the useful case is asking the same request of another instance you are
already observing, and anything wider turns a debugger into a request forge.

**A truncated capture cannot be replayed, and Sonda refuses rather than
trying.** Only the head of the body was stored, so what went out would not be
what was captured, and the result would carry the word "replay" while being a
different request. The refusal names the fix: raise `max_body_bytes` and capture
it again.

### The diff is structural

Bodies are compared as parsed structures, not as text. Reordered keys and
reindented blocks are not differences, so the answer is a short list of paths
instead of a wall of red and green:

```
~ qty          a 3          b 7
+ nota                      b urgente
```

- Array order **is** a difference — position carries meaning in a protobuf
  repeated field — while object key order is not.
- `1290000` and `"1290000"` are the same value. protojson renders int64 as a
  string, and reporting that would be a statement about encoding, not data.
- gRPC streams are compared message by message, so message 3 of 5 can differ
  while the rest match.
- Duration is deliberately excluded: it changes on every replay and would bury
  the differences that mean something.
- When a side is not JSON, or has no schema, the diff says so and reports
  whether the bytes match rather than inventing a structural comparison.


## Comparing two runs

Comparing two calls answers "why did this request fail and that one work". It
does not answer the question people actually arrive with, which is **"this
worked yesterday and today it does not"** — because a flow is a tree of calls,
and the thing that changed is usually several hops down it, or is a call that
stopped happening at all.

Give it one call from each run and it finds the rest of both trees:

```
GET /api/flowdiff?a=1204&b=1731
```

In the interface, open a call from the run that worked, press **HOLD RUN**, then
open a call from the run that failed and press **DIFF FLOW**. In the terminal
client the same two steps are `x` and `x`. An agent calls `diff_flows`.

```
4 matched, 1 only in a, 0 only in b
first divergence: gateway http POST /orders/{} → ms-rates http GET /rates/{}

gateway http POST /orders/{}                                   same
├─ ms-rates http GET /rates/{}                                 changed
│      status: 200 → 500
│  └─ ms-schedules http GET /schedules/{}                      same
└─ ms-billing http POST /invoices                              only in a — this call is no longer made
```

### How two runs are aligned

Not by id and not by trace id: those are exactly what differs between runs. Two
calls are the same call when they agree on **service, protocol, method and the
shape of the path**, and then by their position among the siblings that share
that signature.

- **The shape of the path** means `/orders/ORD-1` and `/orders/ORD-2` are one
  call. A segment becomes a wildcard when it is all digits, a UUID, a long hex
  string, or an identifier with a separator in it. `normalize=loose` also
  wildcards any segment containing a digit, which will flatten `/v2/` and `/v3/`
  into the same route; `normalize=off` compares paths literally.
- **gRPC needs none of that.** Its path is `/package.Service/Method` and carries
  no values, so the signature is exact.
- **GraphQL is aligned by operation**, because every GraphQL request in a
  codebase is a POST to the same endpoint and matching by path would pair a
  mutation with a query.

### Read `unmatched` before you believe the rest

A comparison where most calls found no partner did not find real differences: it
found a path shape that is not being recognised. That is the knob, not a
finding, and the answer says so rather than presenting a wall of missing calls
as though the system had changed.

Two more honesty flags travel with every answer. `same_entry` is false when the
two seeds were not even the same call, which makes everything below meaningless.
`certain` is false when either run was grouped by timing instead of a real trace
id — a difference between two guesses is not the same claim as a difference
between two facts.

### Where a trace id comes from

`certain` being true does not always mean the client instrumented itself. A
request with no trace id of its own leaves everything it causes ungroupable
except by guessing from timing — so Sonda writes one, as `X-Request-Id`, before
forwarding the request onward. It is the one place forwarding is not byte
exact, and it exists for the same reason grouping by timing is a last resort:
a flow with no way to tell one occurrence from the next is worse debugging, not
more faithful capture.

The exception is narrow on purpose. An id already present, however it is
spelled, is never touched — a client's own correlation always outranks a guess
that it needs one. And every call whose `trace_id` Sonda wrote carries
`trace_id_injected: true`, in the API, in the tree (`[trace id from Sonda]`),
and in the interface, so it is never mistaken for something the client sent.

Whether it groups anything beyond the one call Sonda wrote it on depends on the
client: an id only correlates further hops if whatever receives it echoes
`X-Request-Id` onto its own outbound calls, the way many services already do
for their own logs. When it does not, the single call still gets a real id
instead of none, and the rest of the tree falls back to being grouped by timing
as it always did.

### What is compared, and what is not

Per aligned pair: status, whether it failed, the failure detail, and whether one
side was answered from a recording. Duration is deliberately left out, for the
same reason it is left out of a call diff — it changes on every run and would
flag every node.

Payloads are compared only for the divergence and its direct children by
default, using the same structural diff as above. `bodies=all` compares every
aligned pair, which on a wide flow means dozens of payload reads and a wall of
JSON; `bodies=none` skips them.

One limitation worth knowing: siblings that share a signature are paired by
position. Two runs that made the same three calls in a different order will pair
them up wrongly.
