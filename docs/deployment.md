# Deployment

How the Asset Cracker service gets from a commit to running on the Pi, how it is rolled back,
and who is allowed to do what. Companion to `platform-brief.md`.

## The machine

`rpi-v5-1.local` (10.0.0.172, reserved): Raspberry Pi 5, 16 GB, 1 TB NVMe, Debian 12.
PostgreSQL 17 with data checksums, listening on localhost only. The service is never exposed
to the network; its health page is on `127.0.0.1:8377`. Off-LAN access, when the app needs it,
goes through a private VPN.

## One record, one scratch database

| | record | scratch |
|---|---|---|
| Database | `assetcracker` | `assetcracker_dev` |
| Holds | the tape, the strategy registry, the paper ledger | schema rehearsal only |
| Tables owned by | `assetcracker_owner` (no login) | `acdeploy` |
| Who writes | `assetcracker`, under systemd | nobody: the service refuses to start here |
| Migrate | `db/migrate.sh prod` | `db/migrate.sh` (rehearse, then prod) |
| Release | `deploy/pi/deploy.sh prod` | `deploy/pi/deploy.sh` copies a spare binary and does not run it |
| Wipe | the buckets page, simulated books, after typing `reset sim` | `db/reset-dev.sh` |

A strategy is a version row in the record. Graduation is its status (`draft`, `probation`,
`bench`, `active`, `retired`), not a second database. Agents read the record as
`assetcracker_ro`. Both databases are sim-only until the real-trading gates in the brief are met.

## Who can do what

| Account | Is | Can |
|---|---|---|
| `bradl` | admin | anything. Needed only for the one-time installs below |
| `acdeploy` | deploy user, SSH key only | an allowlist of passwordless sudo: pinned apt packages, start/stop/restart/status and logs of the `postgresql` and `assetcracker` units, `psql` as `postgres`. Anything else needs its password, which only Brad knows |
| `assetcracker` | service user, no login | run the binary; connect to `assetcracker` by peer auth |

Database roles in prod:

| Role | Can |
|---|---|
| `assetcracker_owner` | owns every table. Nothing logs in as it; migrations `set role` to it |
| `assetcracker` | select and insert; update only the columns listed in `db/grants.sql`. No delete, no truncate, no DDL, so it cannot disable the triggers that keep the ledger append-only |
| `assetcracker_ro` | read-only. Analysis and the MCP server. `acdeploy` is a member and connects only to take this role |

The agent works only as `acdeploy`. Admin steps are handed to Brad as a command to run.

## One-time setup

1. `deploy/pi/create-deploy-user.sh` (admin) - done 2026-09-20
2. `deploy/pi/setup.sh` (as `acdeploy`) - Postgres, databases - done 2026-09-20
3. `deploy/pi/setup-prod-db.sh` (as `acdeploy`) - the three database roles
4. `deploy/pi/install-service.sh` (admin) - systemd unit, hardened; nightly backup timer

## A release

A release is a commit. The binary carries its short sha and reports it at `/healthz`.

```
deploy/pi/deploy.sh prod
```

1. Refuses unless the tree is clean and the branch is `main` (`AC_ALLOW_BRANCH=<branch>` to
   override while work has not been merged yet).
2. `go vet`, `go test`, cross-compile for linux/arm64.
3. Copies to `/opt/assetcracker/releases/<sha>/`. Needs no sudo: the deploy user owns that tree.
4. `db/migrate.sh prod`: new migrations, then `db/grants.sql`.
5. Points `/opt/assetcracker/current` at the new release (one atomic rename), remembers the
   old one in `/opt/assetcracker/previous`, restarts the unit.
6. Polls `/healthz` for up to a minute. Healthy means: the expected version, the database
   answering, the Coinbase feed alive (some product traded in the last minute; a quiet coin is
   not a fault), quotes under a minute old for every series.
7. **Not healthy: switches back to the previous release, restarts, prints the unit's last log
   lines, exits non-zero.** Healthy: prunes to the newest five releases.

`deploy/pi/deploy.sh rollback` does step 7's switch on demand.

## Migrations

- Numbered SQL files in `db/migrations/`, forward only, each in one transaction, recorded in
  `schema_migration`.
- **Once a file has reached prod it is never edited.** (Before prod existed, `0001` was edited
  in place. That stops now.)
- **Expand, then contract.** A migration must leave the previous release able to run: add
  columns and tables first, stop using the old ones in a later release, drop them in a
  migration after that. This is what makes rolling the binary back safe without touching data.
- `db/test.sh` runs against dev and attempts each rule the database is meant to enforce.

## Backups

Nightly at 03:30 (Pi time): `pg_dump` of `assetcracker` to `/var/backups/assetcracker/`, kept
14 days. **This guards against mistakes, not against losing the disk.** Copying the dumps off
the Pi is not set up: it needs a destination from Brad. These are tax records; decide before
real money.

Restore drill (not yet performed; do one before relying on it):
`pg_restore --create --dbname=postgres <file>` into a scratch cluster, then compare row counts
and `select sum(balance_cents) from ledger_balance` (must be 0 per mode).

## Watching it

The page is served by the Pi. Three roads lead to it; pick by who else can reach the network.

- **The house network** (the plain one): in `/etc/assetcracker/env` set
  `AC_HTTP_ADDR=0.0.0.0:8377` and `AC_OPERATOR_KEY=<a passphrase>`, then restart the unit. The
  page is `http://rpi-v5-1.local:8377/` from any browser in the house. Anyone on the Wi-Fi can
  read it (simulated figures); a change on the buckets page or the assets dialog asks for the
  passphrase once per tab and sends it as `X-Operator-Key`. Without `AC_OPERATOR_KEY` the
  controls are open to whoever can reach the port: fine on localhost or a tailnet, not on a LAN.
  The MCP server's `strategy_register` sends the same header; its copy of the key lives in
  `~acdeploy/.config/assetcracker/operator_key` (mode 600), put there once by the owner, see
  `docs/mcp-read-surface.md`, "The operator key". The service's copy stays in the env file.
- **Tailscale** (anywhere, your devices only): `deploy/pi/install-tailscale.sh`, an admin step run
  once, puts the Pi on your tailnet and publishes the port with `tailscale serve`. The page is
  then `https://rpi-v5-1.<tailnet>.ts.net/` over HTTPS from the laptop or the phone, and
  unreachable from everything else; the tailnet is the login. Never `tailscale funnel`: that is
  the public internet, and Reset is one click.
- **An SSH tunnel** (the fallback): `tools/view.sh` forwards a local port and opens the browser.

By hand:

```
ssh acdeploy@rpi-v5-1.local curl -s http://127.0.0.1:8377/healthz
ssh acdeploy@rpi-v5-1.local sudo journalctl -u assetcracker -n 100 --no-pager
ssh acdeploy@rpi-v5-1.local sudo systemctl status assetcracker
```

systemd restarts the service five seconds after a crash. The home page says so itself when an
engine is halted, the service is unhealthy, the ledger could not be read, or no value snapshot
has been written for five minutes. There is still no PUSH alert: nothing reaches anyone who is
not looking, and nothing at all if the Pi is off. Brad chose to leave that for later
(2026-09-21); ntfy or a Discord webhook, placed on the Pi by him, are the candidates.

## Secrets

There are none today: every request is a public read. When real trading arrives, exchange keys
go in `/etc/assetcracker/env` (root and the service only), put there by an admin by hand.
Never in git, chat, Agora, or a command line. The agent does not handle them.

## Branches and tracking

Work happens on a branch and is pushed as it goes. A step that is finished is merged to `main`
by pull request; prod releases come from `main`. Every piece of work is an Agora task under
INT-10 in the `asset_cracker` room, created when the work starts and moved to done with the
commit that finished it.

## Not done yet

- Off-Pi backup destination, and a restore drill.
- Alerting.
- The Pi boots to a desktop. Booting to the console would free a few hundred MB; optional.
- A UPS or a tested power-loss recovery. Postgres is crash-safe and checksummed, but an
  unclean power cut has not been tried.
