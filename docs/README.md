# Sonda documentation

[← Back to the project](../README.md) · *[En español](es/README.md)*

## Getting it running

| | |
|---|---|
| [Installing Sonda](install.md) | Every install route, and the PowerShell notes for Windows |
| [Projects and configuration](configuration.md) | Reading a project's `.env` or compose file, and the configuration file itself |
| [Interfaces](interface.md) | The web interface and the terminal client |
| [It captures nothing, now what](troubleshooting.md) | The checklist for a service that is pointed at Sonda and shows nothing |

## What it understands

| | |
|---|---|
| [Protocols](protocols.md) | gRPC, TLS, PostgreSQL, AMQP 0-9-1, GraphQL, sockets and event streams |
| [Storage, behaviour and cost](storage.md) | What is written down, what is blanked, what it costs to keep |

## What it does with a capture

| | |
|---|---|
| [Replay and diff](replay.md) | Sending a recorded call again, and comparing two of them structurally |
| [Experiments](experiments.md) | Stub mode, breaking a service on purpose, and contract drift |
| [Coding agents](agents.md) | The MCP server, request trees, and what an agent can and cannot see |
| [HTTP API](api.md) | The API every client reads, including Sonda's own |

## About the project

| | |
|---|---|
| [Related work](comparison.md) | The other tools that capture gRPC, and when to use them instead |
| [Roadmap](roadmap.md) | What is done and what is next |
| [Contributing](../CONTRIBUTING.md) | Tests, benchmarks, layout, protos, commits |
| [Security](../SECURITY.md) | Reporting something that should not be public |
