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

