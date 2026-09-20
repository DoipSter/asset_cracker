#!/usr/bin/env bash
# Prepare the Raspberry Pi to host the Asset Cracker service's database.
#
# Read this before running it. Run it on the Pi, as your normal user (it calls sudo itself):
#
#     bash setup.sh
#
# It is safe to run more than once: every step checks before it acts.
#
# What it does
#   1. Installs PostgreSQL 17 from the PostgreSQL project's own apt repository (apt.postgresql.org).
#      Debian 12 only ships 15, which leaves support in November 2027; these are long-lived
#      records. To use Debian's 15 instead, run:  PG_VERSION=15 bash setup.sh
#   2. Creates a system user "assetcracker" (no login, no home) for the service to run as.
#   3. Creates two databases:
#        assetcracker       owned by role "assetcracker"   the real run
#        assetcracker_dev   owned by role "$USER"          development and tests
#   4. Adds a small memory-tuning file sized for this 16 GB Pi.
#
# What it deliberately does NOT do
#   - Set any password. Both roles log in over the local unix socket by "peer" authentication:
#     Postgres trusts the operating-system user. There is no database password to leak.
#   - Open Postgres to the network. It keeps Debian's default of listening on localhost only,
#     and the script stops if that is not the case. From the Mac, reach the dev database through
#     SSH:  ssh -L 5433:/var/run/postgresql/.s.PGSQL.5432 bradl@rpi-v5-1.local
#   - Touch time sync. systemd-timesyncd was measured active and synchronized on 2026-09-20.
#   - Install the service itself, Go, or anything else.

set -euo pipefail

PG_VERSION="${PG_VERSION:-17}"
SERVICE_USER="assetcracker"
DEV_ROLE="${USER}"

say() { printf '\n==> %s\n' "$*"; }

[ "$(uname -m)" = "aarch64" ] || { echo "expected a 64-bit ARM system, found $(uname -m)"; exit 1; }
[ "$(id -u)" -ne 0 ] || { echo "run this as your normal user, not as root"; exit 1; }

say "Installing PostgreSQL ${PG_VERSION}"
if ! command -v "/usr/lib/postgresql/${PG_VERSION}/bin/postgres" >/dev/null; then
  sudo apt-get update
  sudo apt-get install -y postgresql-common ca-certificates
  if [ "${PG_VERSION}" != "15" ]; then
    # Debian's own helper for adding the PostgreSQL project's signed repository.
    sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
  fi
  sudo apt-get install -y "postgresql-${PG_VERSION}"
else
  echo "already installed"
fi
sudo systemctl enable --now postgresql

say "Checking Postgres only listens on this machine"
listen="$(sudo -u postgres psql -Atc 'show listen_addresses')"
if [ "${listen}" != "localhost" ]; then
  echo "listen_addresses is '${listen}', expected 'localhost'. Stopping so nothing is exposed."
  exit 1
fi
echo "listen_addresses = ${listen}"

say "Creating the service's system user"
if ! id "${SERVICE_USER}" >/dev/null 2>&1; then
  sudo adduser --system --group --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}"
else
  echo "already exists"
fi

role() {  # role NAME : create a login role with no password if it is missing
  sudo -u postgres psql -Atc "select 1 from pg_roles where rolname='$1'" | grep -q 1 \
    || sudo -u postgres createuser --no-superuser --no-createdb --no-createrole "$1"
}
database() {  # database NAME OWNER
  sudo -u postgres psql -Atc "select 1 from pg_database where datname='$1'" | grep -q 1 \
    || sudo -u postgres createdb --owner="$2" --encoding=UTF8 "$1"
}

say "Creating roles and databases"
role "${SERVICE_USER}"
role "${DEV_ROLE}"
database assetcracker "${SERVICE_USER}"
database assetcracker_dev "${DEV_ROLE}"
# Nobody but the owner may connect to either database.
sudo -u postgres psql -qc "revoke connect on database assetcracker from public"
sudo -u postgres psql -qc "revoke connect on database assetcracker_dev from public"

say "Memory settings for a 16 GB Pi shared with the service"
conf="/etc/postgresql/${PG_VERSION}/main/conf.d/assetcracker.conf"
if [ ! -f "${conf}" ]; then
  sudo tee "${conf}" >/dev/null <<'CONF'
# Written by asset_cracker deploy/pi/setup.sh. Delete this file and restart to undo.
shared_buffers = 2GB
effective_cache_size = 8GB
maintenance_work_mem = 512MB
work_mem = 32MB
random_page_cost = 1.1          # NVMe: random reads cost about the same as sequential
timezone = 'UTC'                # store everything in UTC; clients convert for display
CONF
  sudo systemctl restart postgresql
else
  echo "already present: ${conf}"
fi

say "Done"
sudo -u postgres psql -Atc "select version()"
sudo -u postgres psql -c '\l assetcracker*'
psql -d assetcracker_dev -Atc "select 'dev database reachable as ' || current_user"
