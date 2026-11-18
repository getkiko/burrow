# burrow-signal installer

`install.sh` installs and manages `burrow-signal`, the Burrow signaling server,
on a Linux host with systemd. It downloads the binary from the
[GitHub releases](https://github.com/getkiko/burrow/releases) of
`getkiko/burrow`, verifies it against the release's `SHA256SUMS`, writes a
systemd unit, opens the firewall, starts the service and checks the endpoints.
Running it again upgrades to the latest release.

Supported platforms: `linux/amd64` and `linux/arm64`. The server needs a public
IP address; it only introduces peers to each other and never relays traffic.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/getkiko/burrow/main/signal-deploy/install.sh | sudo bash
```

When a terminal is available the script opens an interactive menu, even when
piped from curl (prompts read `/dev/tty`). Choose **Install** and answer the
prompts, or press Enter to accept the defaults.

For an unattended install, set `ASSUME_YES=1` and pass the configuration as
environment variables:

```bash
curl -fsSL https://raw.githubusercontent.com/getkiko/burrow/main/signal-deploy/install.sh | \
  sudo ASSUME_YES=1 BURROW_SIGNAL_PREFIX=/signal BURROW_SIGNAL_SECRET="$(openssl rand -hex 32)" bash
```

## What gets installed

| Path | Purpose |
|---|---|
| `/usr/local/bin/burrow-signal` | server binary (release asset `burrow-signal-linux-<arch>`) |
| `/usr/local/bin/burrow-signal-menu` | this script, to open the management menu later |
| `/etc/burrow-signal/burrow-signal.env` | configuration (mode 600) |
| `/etc/burrow-signal/version` | installed release tag |
| `/etc/systemd/system/burrow-signal.service` | systemd unit |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `BURROW_SIGNAL_LISTEN` | `:9960,:9962,:9963,:10002` | comma-separated listen addresses (`-listen`) |
| `BURROW_SIGNAL_STAT` | `127.0.0.1:9961` | metrics endpoint `/stat` (`-stat`); empty disables it |
| `BURROW_SIGNAL_PREFIX` | empty (root path) | path prefix of the entry point (`-prefix`), e.g. `/signal` |
| `BURROW_SIGNAL_SECRET` | empty | HMAC key for probe tokens; when empty a random key is generated at every start |
| `BURROW_SIGNAL_VERSION` | empty (latest) | pin a release tag, e.g. `v0.1.0` |

Precedence is environment variable, then the existing configuration, then the
default. Set `FORCE=1` to download the binary again even if that version is
already installed.

Setting `BURROW_SIGNAL_SECRET` keeps probe tokens valid across restarts, and
lets several server instances accept each other's tokens.

Change the configuration with **Change configuration** in the menu. It rewrites
the systemd unit (the flags are part of `ExecStart`), updates the firewall,
closes ports that are no longer used and restarts the service. Editing the env
file by hand has no effect until you run that menu item or reinstall.

## Management

Run `sudo burrow-signal-menu`:

| Item | What it does |
|---|---|
| Reinstall / upgrade | install the latest (or pinned) release, keeping the configuration |
| Change configuration | edit listen addresses, metrics, prefix, secret and pinned version |
| Show configuration and status | version, settings (never the secret), service state, ports, firewall, health check |
| Start / stop / restart | service control |
| View logs | last 100 lines, or follow with `journalctl -f` |
| Update | upgrade to the latest release unless a version is pinned |
| Uninstall | remove the service, firewall rules, binary, menu script and `/etc/burrow-signal` |
| Firewall | show port state, reopen all ports, or remove the rules added by the script |

The usual systemd tools work as well: `systemctl status burrow-signal`,
`journalctl -u burrow-signal -f`.

## Firewall

The script uses the first available backend in this order and only opens TCP
ports:

`firewalld` -> `ufw` -> `nftables` -> `iptables`

Rules are tagged with the comment `burrow-signal` so they can be removed
exactly. Ports bound only to loopback (such as the `127.0.0.1:9961` metrics
endpoint) are not opened. Rules are persisted where possible
(`netfilter-persistent`, `/etc/nftables.conf`, `/etc/iptables/rules.v4`); the
script warns when it cannot persist them.

On a cloud server you also need to allow the ports in the provider's security
group; the script cannot do that for you.

## Endpoints

```
ws://<public-ip>:<port><prefix>?command={signal|echo|nat}
```

`GET <prefix>?command=health` returns 204. After starting, the installer checks
every port for `health=204` and a WebSocket upgrade (`101`) on `command=nat`.

## Requirements

- root and systemd
- `curl` or `wget` (`curl` is also used for the endpoint checks)
- `sha256sum` or `shasum` to verify downloads

The installer itself needs root, but the service does not: the unit uses
`DynamicUser=yes`, keeps only `CAP_NET_BIND_SERVICE` (for ports below 1024), and
runs with `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, a system call
filter and related hardening. It reads and writes no files at runtime; systemd
reads the root-only environment file before dropping privileges.

The unit adapts to the systemd version (`systemctl --version`):

- **235 or newer**: `DynamicUser=yes`. Older systemd would ignore it and start
  the service as root, so there the installer creates a `burrow-signal` system
  user and group (no login shell, marked in its GECOS field) and sets
  `User=`/`Group=`. Uninstall removes only an account carrying that marker. If an
  account named `burrow-signal` already exists and was not created by the
  installer, it is used only if it is a system account without a login shell;
  otherwise the installer stops.
- **239 or newer**: `SystemCallFilter=@system-service`. Older systemd does not know
  that group but would still apply the allow-list and kill the service at
  start, so the filter is left out there.
- **229 or newer**: `AmbientCapabilities`. Older systemd cannot grant the
  capability, so the installer warns if a configured port is below 1024.

Other hardening settings that an older systemd does not know are skipped with a
warning in the journal; this only weakens the sandbox and does not stop the
service. The unit uses `Restart=always`, and stops restarting after 10 restarts in
5 minutes so a crash loop does not flood the journal.

## Releases

Release assets are built by `scripts/release.sh` in the repository root:
`burrow-signal-linux-amd64`, `burrow-signal-linux-arm64`, the `burrow` CLI for
Linux and macOS, and `SHA256SUMS`.
