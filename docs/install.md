[← Docs](README.md)

# Installing Sonda

## Install

Pick whichever you already have. Nothing here needs a C toolchain or a system
SQLite: the driver is pure Go, which is also why the binaries are static and the
image is 50 MB.

| | |
|---|---|
| **macOS** | `brew install NicolasCondezaR/tap/sonda` |
| **Windows** | `scoop bucket add nicolascondezar https://github.com/NicolasCondezaR/scoop-bucket`<br>`scoop install sonda` |
| **Go** | `go install github.com/NicolasCondezaR/sonda/cmd/sonda@latest` |
| **Docker** | `docker run -p 127.0.0.1:9000:9000 -v sonda:/data ghcr.io/nicolascondezar/sonda` |
| **Binary** | Download from [Releases](https://github.com/NicolasCondezaR/sonda/releases), unpack, run |
| **Source** | `git clone` and `go build ./cmd/sonda` |

On Linux, use `go install`, the image, or the tarball: Homebrew casks are macOS
only.

The Docker line publishes to `127.0.0.1` rather than to every interface, and
every other one here does the same: `sonda.db` holds whatever credentials
crossed the wire, in plaintext and behind no login. Inside the container Sonda
binds `0.0.0.0` — the isolation is the container's job — which is what
`-api-listen` in the image's command does; outside one, the default stays
loopback.

That Docker line publishes the interface and nothing else, which is enough to
look around and not enough to capture anything: **a proxy needs its own
published port per service**, because the port a client connects to is the
whole mechanism. Adding a service on `9101` and then finding that nothing on
your machine can reach it is the confusing half hour this paragraph exists to
prevent.

```bash
docker run -p 127.0.0.1:9000:9000 -p 127.0.0.1:9101:9101 \
  -v sonda:/data ghcr.io/nicolascondezar/sonda
```

`docker compose up` publishes 9101 and 9201 already, which is why the quick
start below works without saying any of this.

The release archives carry four binaries, not one: `sonda`, the terminal client
`sonda-tui`, and the two toy services `echo` and `grpcdemo` that the quick start
below uses — so there is something to capture without wiring up your own. The
package managers install only the first two, since a binary named `echo` on the
PATH has no business shadowing the system one.

```bash
sonda            # the proxy and the interface, on http://127.0.0.1:9000
sonda -version   # which build this is
sonda-tui        # the terminal client
```

Downloading the tarball by hand on macOS has one extra step: Gatekeeper
quarantines anything unsigned that arrives through a browser. Either fetch it
with `curl`, or clear the flag once. Homebrew does this for you.

```bash
xattr -dr com.apple.quarantine sonda sonda-tui echo grpcdemo
```

### Start Sonda when you sign in to Windows

On Windows, Sonda can install one Task Scheduler entry for the current user.
It runs at ordinary user privilege, only after that user signs in, and stores
no password:

```powershell
sonda autostart install -config "$HOME\.sonda\sonda.yaml"
sonda autostart status
```

`install` creates the task, or reuses it only when its complete definition still
matches the current user and configuration. It never replaces a foreign or
modified task at Sonda's deterministic name. It starts the verified task
immediately and waits for `/health`; no restart is needed to check the setup.
The usual lifecycle is:

```powershell
sonda autostart start
sonda autostart stop
sonda autostart restart
sonda autostart uninstall
```

If installation used a non-default configuration path, pass that same
`-config PATH` to `status`, `start`, `stop`, `restart`, and `uninstall`. Those
commands derive the expected task locally instead of trusting metadata stored
inside the task.

The managed task sets the configuration path and working directory absolutely,
runs on battery, keeps one instance, has no 72-hour limit, starts when available,
and retries a failed process three times at one-minute intervals. When the
running binary is the exact `apps/sonda/current` target of a verified Scoop
shim, it uses that shim so `scoop update sonda` does not strand the task on an
old version. A binary run from a release ZIP or local build is treated as
portable: put it in its final location before installing autostart. If it is
moved later, restore the original path or remove the old task manually in Task
Scheduler before installing again; Sonda will not overwrite the changed action.

Stopping signals only the Sonda instance identified by this task and lets its
capture queue drain. If graceful signaling fails while the canonical task is
unchanged and its expected control event still proves that the managed process
is active, the sole fallback is to end that exact scheduled task; status reports
that the fallback was abrupt. Sonda never kills every process named `sonda.exe`.

Autostart refuses a configuration whose `api_listen` is not loopback. The
explicit `-allow-non-loopback` override exists for isolated environments, but
it exposes an unauthenticated API that can read captures and change proxy state.
`uninstall` removes only the task; configuration, database, certificate
authority and logs remain in place. Logs go beside the configuration as
`sonda.log`, with one bounded rotated copy.

## PowerShell

PowerShell 5.1 rewrites quotes when passing arguments to external executables,
so `curl.exe` silently mangles JSON bodies:

```powershell
# WRONG — the upstream receives {sku:ABC-9}, quotes stripped
curl.exe -X POST -H "Content-Type: application/json" -d '{"sku":"ABC-9"}' http://127.0.0.1:9101/echo
```

Use `Invoke-RestMethod`, or put the body in a file:

```powershell
# Sends a body, and reads what Sonda captured
$body = @{ sku = 'ABC-9'; qty = 3 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:9101/echo -ContentType 'application/json' -Body $body

(Invoke-RestMethod -Uri http://127.0.0.1:9000/api/calls).calls |
  Select-Object id, method, path, status, duration_ms | Format-Table

$id = (Invoke-RestMethod -Uri 'http://127.0.0.1:9000/api/calls?q=ABC-9').calls[0].id
(Invoke-RestMethod -Uri "http://127.0.0.1:9000/api/calls/$id").request.text
```

```powershell
# curl.exe is fine when the body comes from a file
curl.exe -X POST -H "Content-Type: application/json" --data-binary '@body.json' http://127.0.0.1:9101/echo
```

