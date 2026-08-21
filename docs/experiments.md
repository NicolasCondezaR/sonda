[← Docs](README.md)

# Experiments

## Stub mode

Sonda already holds the exact bytes of every response a service ever gave. Handing
them back instead of forwarding turns the same tool into something else:

- Work on the front while its backend is down
- Run a test without twenty-one processes
- Reproduce a bug from a capture, on a laptop, without the environment that made it

```bash
curl -X POST http://127.0.0.1:9000/api/stub   -d '{"service":"ms-rates","enabled":true}'
```

From an agent, `set_stub` does the same. It asks first — a service quietly
answering from last week is exactly the kind of surprise worth confirming.

### Why it cannot be mistaken for the real thing

A recorded answer that passes for a live one is the failure this feature has to
avoid, so four things make that hard to do by accident:

- Every stubbed response carries **`X-Sonda-Stub: <call id>`**
- The exchange is still captured, linked back to the recording it came from, so
  the field never shows traffic that never happened as though it had
- **Stubbing is forgotten when Sonda restarts.** It is never written to the
  database: a stub that outlives a restart is one nobody remembers turning on
- A request with no recording gets a **501 that explains itself**, not an
  invented answer and not a silent empty 200

### Which recording answers

An identical request body wins outright — that is the difference between
replaying *the answer to GetOrder* and *the answer to GetOrder(ORD-1)*, and a
test handed somebody else's order is worse off than one handed an error. Failing
that, the most recent call to the same method and path.

Captures that were themselves stubbed are never reused. Without that, leaving
stubbing on would slowly feed Sonda its own answers.

gRPC works too: the recorded trailers are replayed, so the client gets the real
`grpc-status` instead of waiting for one that never comes.

## Breaking things on purpose

Retry logic, timeouts and degradation are written once and then never
exercised, because making a real service fail on demand is awkward enough that
nobody does it. Sonda is already in the path of every call.

```bash
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-rates","latency_ms":2000}'
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-seo","status":503,"one_in":3}'
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-auth","cut":true}'
```

From an agent, `break_service` does the same, and it asks first.

In the interface it is on the service itself, under **PROJECTS**: the row
carries `LATENCY MS`, `HTTP STATUS`, `CUT` and `ONE CALL IN` beside an `ARM`
key, and reads back **BROKEN ON PURPOSE** with the rule in force until
`RESTORE` takes it off. A rule that would do nothing — no latency, no status,
no cut — is refused, and what the panel shows is the refusal rather than a rule
that was never armed.

The terminal reads the same state and does not set it, which is the level it
already works at for stubbing: the bar counts what is armed, the channel
carries the fault block before its name, and the inspector names the rule
beside the call being read.

**Latency lets the call through** — the service still answers, it just takes
longer, which is the case a timeout is meant to catch. **A status or a cut ends
the call at Sonda**: the service is never reached.

### Deterministic, not random

`one_in: 3` means one call in every three, in that order, every run. A
percentage would behave differently each time and turn a failing test into a
coin toss; a sequence you can reproduce is the only kind worth debugging
against. Changing a rule restarts its schedule.

### It can never pass for a real failure

Every injected failure carries **`X-Sonda-Fault`** with the reason, is recorded
as injected, and is marked as such in the field, the inspector and the terminal.

A rule in force is stated where the failures are being read, not only where it
was armed: the channel shows **BROKEN**, and the readout at the top of the
browser and the bar in the terminal both count what is armed. That matters most
above `one_in: 1`, where most calls pass through untouched and the injected ones
on their own look like a service that is merely flaky.

Rules are forgotten when Sonda restarts, for the same reason stubbing is: a
service that has been failing since Tuesday because of a rule nobody remembers
setting is a worse afternoon than the bug being chased. Nothing about them is
written to the database.

## Contract drift

In a monorepo where nobody versions a contract, a field that quietly went away
or changed type breaks the caller days later, far from the change that caused
it. Sonda already holds every response a service ever gave.

```
CONTRACT                                vs capture #412
−  data.items[].currency                    was string
~  data.total                          number -> string
+  data.meta.cached                              boolean

2 of these would break a caller.
```

It compares **shapes, not values**. Two calls returning different prices are not
drift; one returning a price as a number and the other as a string is.

- The baseline is the **oldest capture Sonda holds** of the same endpoint, not a
  schema someone has to maintain — a baseline nobody keeps up to date is a
  baseline that is gone in three weeks.
- A list collapses to the shape of its items. Two hundred orders report the
  shape of one order, or the field that changed would be buried under itself.
- A **nullable field is not drift.** Flagging every one of them buries the
  changes that matter under noise nobody can act on.
- An empty list claims nothing about what it holds. Guessing would invent a
  contract nobody wrote.
- **Adding a field is safe**; losing one or changing its type is what takes a
  caller down, and the report says which is which.

In the interface it is a section of the inspector, in the terminal it is `c`,
and for an agent it is `contract_drift`. This is the one thing in Sonda that
never touches the proxy: it only reads what was already stored.

