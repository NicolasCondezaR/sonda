---
name: Something is wrong
about: A capture that is missing, wrong, or misleading
labels: bug
---

**What you expected Sonda to show, and what it showed instead.**

**The traffic involved:** protocol (HTTP, gRPC, WebSocket), and whether the
service serves reflection or needed a descriptor set.

**How to reproduce it.** The two toy services in `examples/` produce real
capturable traffic on first run, and a reproduction built on those is worth ten
built on a private stack.

**Version:** the output of `sonda -version`.

Please do not attach `sonda.db` or a raw capture. They contain whatever your
traffic carried, including credentials.
