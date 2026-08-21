[← Docs](README.md)

# Interfaces

## The interface

Sonda reads as a logic analyzer rather than a request table, because a table
answers "what happened" and never "what was happening at the same time" — which
is the question when fifteen services are talking and one of them broke.

- **Channel rail.** One row per target, carrying its colour from the logic-probe
  code, its total call count and its fault count. The counts are unfiltered: the
  rail answers "is this service healthy", so a filter in front of the field must
  not change it.
- **Event field.** One lane per channel against a live time axis whose right
  edge is now. A call is a mark whose width is its duration; **a fault is a
  different shape** — a full-height bar — so it survives a red channel, a
  colour-blind reader and a glance from across the room.
- **Cursors.** Two of them, `A` and `B`, the way the instrument this reads as
  labels them. Select a call and press `a` or `b` — or click the control — and a
  hairline crosses every channel at that call's place in time; with both down the
  bar reads the span between them. Pressing the same key again lifts it.
- **Inspector.** The selected call, decoded, with its schema source named.

It opens filtered to faults, because that is why you opened it. `ALL` switches
to the whole field. Resting the pointer in the field **holds the trace** so a
mark stops sliding while you aim at it; move away and it resumes.

A cursor pins to a **call**, not to a position on screen. The field is a window
sliding on its own, so a cursor parked at an x would measure the view's
pixel-to-time scale rather than the traffic: every span it reads is the
difference between two recorded timestamps, and it stays the same number when
you change the sweep. The reading is start to start — each call's own duration is
already on screen as the width of its mark — and the arrow says which cursor is
earlier, so there is never a negative span to interpret. A cursor whose call
scrolls out of the window is lifted rather than left pointing at nothing.

`FIND` searches paths and payload text, including payloads Sonda only has as
bytes. `/` focuses it, `Escape` closes the inspector.

The whole interface is embedded in the binary — no Node, no build step, no
network requests, no webfonts.

![A gRPC failure: protobuf decoded through reflection, with the real status](assets/sonda-grpc-inspector.jpg)

Above: a gRPC call that returned `PermissionDenied`. The HTTP status is 200 —
gRPC reports failure below HTTP — and the request is decoded to field names
because the service serves reflection.

## The terminal client

The same instrument, in a terminal. It is a second client of the API rather than
a second implementation: it captures nothing and stores nothing, it reads a
running Sonda.

```bash
go build -o sonda-tui ./cmd/sonda-tui
./sonda-tui                          # defaults to http://127.0.0.1:9000
./sonda-tui -api http://host:9000

docker compose run --rm tui            # or from the image
```

```
S O N D A  ■ LIVE   FAULTS  ALL    1M  5M  30M                      19 CAPTURED  ·  2 FLAGGED
CHANNEL       CALLS FAULT │-30M         -25M        -20M        -15M        -10M       -5M  NOW
 ■ echo       7     1     │·············│···········│···········│···········│·········█·····
▸■ orders     12    1     │·············│···········│···········│···········│·········█·····
──────────────────────────┴─────────────────────────────────────────────────────────────────
 POST /demo.v1.Orders/Fail
 orders   gRPC   HTTP 200   1.72ms
 gRPC 7 PermissionDenied — no tienes acceso a este pedido
 demo.v1.Orders / Fail   schema from reflection
 REQUEST  1 message(s)
   {
     "code": 7,
     "message": "no tienes acceso a este pedido"
   }
 RESPONSE  0 message(s)
 ↑↓ chan · ←→ call · g/G ends · ⏎ read · esc close · t tree · c contract · r replay · d diff · f faults · w window · h hold · / find · q quit
```

The translation is mostly direct — monospace is free here, hairlines become
box-drawing characters, and channel colours carry over unchanged. Two things
needed a different expression:

- There are no type sizes, so the four roles become weight and dimming.
- A lane is one row tall, so a fault cannot be a taller bar. It becomes a **full
  block where an ordinary call is a half one** (`█` against `▄`), with a third
  glyph for a cell holding both. Shape still carries the outcome before colour
  does, which is the rule that matters.
- A service being **broken on purpose** is a mode and not a call, so the same
  block is engraved on the channel, before its name, and the bar counts how many
  are armed — the browser's badge and readout in the two places this client has
  for them.

| Key | |
|---|---|
| `↑` `↓` or `k` `j` | pick a channel |
| `←` `→` or `H` `L` | step along it, call by call |
| `home` or `g` | jump to the oldest call on the channel |
| `end` or `G` | jump to the newest |
| `enter` | read the selected call |
| `a` / `b` | place a measurement cursor on the selected call, or lift it; with both down the bar reads the span between them |
| `esc` | close whatever is open: inspector, diff, tree or contract |
| `t` | show the whole request it belonged to, as a tree |
| `c` | has this endpoint changed shape since it used to work |
| `r` | replay it |
| `d` | diff a replay against its original |
| `f` | faults only, or everything |
| `w` | cycle the sweep |
| `h` | hold the trace |
| `/` | search; `enter` applies it, `esc` clears it and leaves the field |
| `q` or `ctrl+c` | quit |

Stepping moves call by call rather than cell by cell: an empty cell is not
something to point at. `h` holds the trace for the same reason the web client
freezes the field under the pointer — a mark that slides while you aim at it is
not selectable.

