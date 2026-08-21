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


## The trigger

Cursors measure what you already caught. They do nothing for the failure that
happens twice an hour while you are looking somewhere else, which is the one
people open a debugger for. Name the condition, walk away, and come back to the
moment it fired.

```
POST /api/trigger   {"service":"ms-rates","failed":true}
GET  /api/trigger
POST /api/trigger   {"clear":true}
```

In the interface, open the call that just broke and press **TRIGGER ON THIS**:
it arms on failures from that service, because every field a form would have
asked for is already on screen. An agent calls `arm_trigger` and reads what it
caught from `list_services`, beside the other armed switches. The terminal
client shows an armed trigger and its firing in the status bar but does not arm
one — the same restraint it already shows with injected faults.

### What it can wait for

`service`, `method`, `path` (a substring, the way the search field works),
`protocol`, `status`, and `failed` — which takes three states: `true` only
failures, GraphQL errors under HTTP 200 included; `false` only calls that did
not fail, which is how you wait for a fix to land rather than for the next
break; and absent, which fires on either.

A condition with nothing in it is refused rather than armed. It would fire on
the next call whatever it was, which is indistinguishable from a bug in the
matching.

### Two modes, and one moment

`single`, the default, disarms itself when it fires and keeps that moment
readable. That is what makes "tell me when this happens again" usable: the
answer is still there when you come back to it. `normal` stays armed and counts
every crossing, which is noisier by design and useful while narrowing something
down.

There is no history of every firing. The field already shows the calls, and a
second list of them would be a second record to keep honest.

### What firing does, and what it will not do

It records: the moment, the call that crossed, and the condition. Everything
else is a consequence each surface applies. The web holds the field and selects
the call. The terminal says so in the bar. An agent reads it whenever it next
looks.

**A trigger never takes the view from someone who is reading it.** If the field
is already held by hand, the trigger records and says so but does not move
anything.

### Three things to know before you rely on it

- **It never matches backwards.** Only calls captured after it was armed can
  fire it, to the nanosecond. A trigger that reached back would answer with
  something that had already happened.
- **It is not persisted.** A restart disarms it, the same as stubs and injected
  faults. An instrument that came back from a restart still armed would fire on
  something nobody was waiting for any more.
- **There is one trigger, not one per service.** Faults and stubs are armed per
  service because they act on a service; a trigger acts on the instrument.
