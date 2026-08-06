---
name: Mirador
description: A logic analyzer for service traffic — calls are events on per-service channels against a live time axis.
colors:
  case: "#14171c"
  field: "#0f1216"
  raised: "#1b1f26"
  grid: "rgba(214, 218, 224, 0.055)"
  grid-major: "rgba(214, 218, 224, 0.11)"
  rule: "rgba(214, 218, 224, 0.14)"
  ink: "#e6e9ee"
  ink-dim: "#9aa3ae"
  ink-faint: "#78828d"
  fault: "#ff5257"
  armed: "#5fa858"
  shadow-overlay: "-12px 0 32px rgba(0, 0, 0, 0.5)"
  ch-brown: "#8a5a3c"
  ch-red: "#c8443c"
  ch-orange: "#d97b2b"
  ch-yellow: "#d8b843"
  ch-green: "#5fa858"
  ch-blue: "#4a86c8"
  ch-violet: "#8b6bc4"
  ch-grey: "#8a8f98"
  ch-white: "#d6dae0"
typography:
  readout:
    fontFamily: "ui-monospace, 'Cascadia Mono', 'SF Mono', Consolas, 'Liberation Mono', monospace"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.45
    letterSpacing: "normal"
  label:
    fontFamily: "{typography.readout.fontFamily}"
    fontSize: "10px"
    fontWeight: 600
    lineHeight: 1
    letterSpacing: "0.09em"
  meta:
    fontFamily: "{typography.readout.fontFamily}"
    fontSize: "11px"
    fontWeight: 400
    lineHeight: 1.4
    letterSpacing: "normal"
  data:
    fontFamily: "{typography.readout.fontFamily}"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: 1.55
    letterSpacing: "normal"
rounded:
  none: "0"
  machined: "2px"
spacing:
  hair: "2px"
  tight: "6px"
  base: "12px"
  loose: "20px"
  bay: "32px"
components:
  control:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.ink-dim}"
    typography: "{typography.label}"
    rounded: "{rounded.machined}"
    padding: "6px 10px"
  control-engaged:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.ink}"
    rounded: "{rounded.machined}"
    padding: "6px 10px"
  event-mark:
    height: "9px"
    rounded: "{rounded.none}"
  event-fault:
    backgroundColor: "{colors.fault}"
    width: "2px"
    rounded: "{rounded.none}"
---

## Overview

Mirador is an instrument, and the interface is the instrument's face. The
organizing metaphor is a **logic analyzer**: each observed service is a channel,
each captured call is an event on that channel, and everything is plotted
against a live time axis that advances on its own.

This is a deliberate refusal of the arrangement every debugging tool ships — the
sortable table with a waterfall column bolted onto the side. A table answers
"what happened"; it does not answer "what was happening at the same time", which
is the actual question when fifteen services are talking and one of them broke.

Two consequences bind everything below. **Nothing may imply the tool altered the
traffic**, so no interpolation, no smoothing, no rounded-off values. And **guesses
are labelled**, so an inferred field type or a heuristic reads visibly as an
inference rather than as measurement.

## Colors

Dark, and not by category habit. The use scene forces it: this sits on a second
monitor for hours beside a dark terminal and Neovim, glanced at rather than
read. A white panel in that position is glare.

The ground is **graphite**, not near-black: `case` for the instrument body,
`field` for the measurement area recessed inside it. The distinction is
structural — the field is where measurement happens and nothing decorative may
enter it.

**Channel colors are the logic-probe colour code in its standard order**, minus
black, which is unreadable here. This is not a palette choice; it is the
ordering a developer who has wired a probe already knows, and it solves a real
problem — fifteen simultaneous services each need an identity the eye can latch
onto. Past nine targets the sequence repeats with a hollow mark, the way a
second probe group is distinguished on a real instrument. Channel colors are
assigned by configuration order and are stable across restarts.

`fault` is reserved. It marks failure and nothing else — never a heading, never
a border, never emphasis. A channel that happens to be red must remain
distinguishable from a fault, which is why **failure is carried by shape first**
(see Components) and by color second.

Text tints from the ground rather than going gray-on-gray: `ink` for readouts,
`ink-dim` for labels and secondary data, `ink-faint` only for information that
would not be missed if it were absent.

**No glow. No bloom. No neon.** Bench instruments do not glow; screens
photographed in the dark do. A colored halo with no offset is the tell that a
dark interface was decorated rather than designed.

## Typography

**Monospace throughout, including labels and chrome.** This is the one place the
"mono as technical costume" prohibition does not apply: every value on this
surface is data, a measurement, or an identifier, and instrument panels are
monospaced end to end. Mixing a proportional UI face into a measurement field
would break column alignment, which is the property that makes the field
scannable.

The stack is the system's own — Cascadia Mono, SF Mono, Consolas — with no
webfont. A tool that must run from a single binary on any machine cannot depend
on a font request.

Four roles, and no more — a fifth step is drift, not a ramp. `label` is small,
semibold and tracked, for the fixed engravings on the instrument: axis ticks,
column headers, control names, the protocol badge. `readout` is the default.
`meta` is one step down, for secondary readings that sit beside a primary one:
channel counts, the top-bar tally, notes. `data` is for payloads, with generous
line height because it is read rather than scanned.

## Layout

Three vertical bays under one measurement bar:

- **Channel rail** (left, fixed): one row per target, with its probe color, live
  count and fault count. This is navigation, and at fifteen-plus targets it is
  the primary one.
- **Event field** (center, fluid): lanes aligned to the rail, a fixed grid, and
  a time axis whose right edge is now. Events scroll leftward out of the window.
- **Inspector** (right): the selected call, decoded.

The grid is real. Vertical hairlines mark time divisions and stay fixed while
events move under them; horizontal hairlines separate channels. It is a
measurement grid, not texture, so its divisions always correspond to the labelled
axis.

Responsive: below 1100px the inspector becomes an overlay over the field, since
reading a payload and scanning the field are not simultaneous tasks. Below 720px
the channel rail collapses to a horizontal strip above the field. The field
itself never disappears — it is the surface.

## Elevation & Depth

Almost none, deliberately. The instrument is machined, not layered: separation
comes from **hairline rules and ground shifts**, not from shadow.

The one exception carries real offset and blur, never a zero-offset halo: the
inspector when it overlays the field on a narrow viewport. Everything else sits
flat in its bay, and a selected event is marked by an outline rather than by
being lifted.

## Shapes

Square by default. `machined` (2px) is the maximum radius and applies only to
controls, the way a physical button has a broken edge rather than a rounded
body. Pills, capsules and soft cards do not belong to this world.

Rules are 1px, always. A colored rule thicker than a hairline reads as
decoration here.

## Components

**Event mark.** A short filled rect in the channel color, width proportional to
duration with a 3px floor so an instant call stays visible. This is the resting
form.

**Fault.** Not a red version of the event mark — **a different shape**: a
full-lane-height vertical stroke in `fault`. Shape carries it so it survives a
red channel, a color-blind reader, and a glance from across the room. This is
the single most important visual rule on the surface, because finding the
failure is why the tool is open.

**Channel row.** Probe swatch, name, then counts right-aligned in a fixed
column so the numbers form a scannable rule down the rail. Hollow swatch past
the ninth target.

**Controls.** Engraved labels, not buttons with verbs. Engaged state is a shift
in ink and a hairline underline, the way a latched toggle shows position — never
a filled accent pill.

**Inspector.** Fixed field/value columns. Decoded payloads render as data with
their schema source named; a structural wire-format view renders its inferences
with the note attached, visibly weaker than measured values.

**Empty field.** Grid and lanes stay drawn with the axis live. The readout says
what is true — armed, and how many calls were captured with none flagged. An
empty state here is a health reading, not an absence.

## Do's and Don'ts

**Do** keep the field advancing on its own. The trace moving is the instrument's
native motion and the one authored moment on the surface; a new event lands
bright and settles into its channel colour, the way a phosphor strike decays.
That flash is a colour change, never a glow — a halo would break the same rule
Colors sets out. Under `prefers-reduced-motion` the field steps forward once a
second instead of scrolling.

**Do** let color do structural work — channel identity, fault — and nothing else.

**Do** show truncation, dropped captures and missing schemas explicitly. A gap
the tool knows about and hides is worse than the gap.

**Don't** add a second accent color. The probe code plus `fault` is the whole
system.

**Don't** animate the interface. Instruments do not ease their panels; only the
trace moves.

**Don't** introduce cards, pills, gradients, glass, or shadow as separation.
Hairlines and ground shifts do that work.

**Don't** round, abbreviate or humanize a measured value in the field. `1201ms`
is the reading; "about a second" is a lie about what was measured.
