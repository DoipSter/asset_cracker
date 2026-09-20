#!/usr/bin/env bash
# Create "acdeploy": the one account used to set up and deploy Asset Cracker on the Pi.
#
# Run it from the Mac, as an admin user on the Pi (it needs full sudo once, to create the account):
#
#   ssh bradl@rpi-v5-1.local "PUBKEY='$(cat ~/.ssh/id_ed25519.pub)' bash -s" < deploy/pi/create-deploy-user.sh
#
# What acdeploy can do
#   - Log in by SSH key only. It has no password until you give it one.
#   - Run the exact commands listed below with sudo, without a password.
#   - Anything else under sudo asks for acdeploy's password. Until you set one (on the Pi:
#     sudo passwd acdeploy) that means "refused". Set one only you know and the rule becomes:
#     routine work is passwordless, everything else needs you.
#
# Why a list of allowed commands and not a list of forbidden ones: a forbidden list cannot be
# made safe. "Everything except rm -rf" still allows sudo bash, sudo env, an editor's shell
# escape, and a hundred other ways to the same place. Only naming what IS allowed holds.
#
# What the allowed commands still imply (be clear-eyed about it)
#   - psql as "postgres" is the database superuser. acdeploy administers the database.
#   - The apt lines are pinned to specific packages, so they cannot install arbitrary software.
#   - Service control is pinned to the postgresql and assetcracker units.
#
# Safe to run again: it rewrites the sudoers file and leaves the account alone.

set -euo pipefail

DEPLOY_USER="acdeploy"
PG_VERSION="${PG_VERSION:-17}"
: "${PUBKEY:?pass the SSH public key to install, as PUBKEY=...}"
case "${PUBKEY}" in ssh-ed25519\ *|ssh-rsa\ *|ecdsa-*) ;; *) echo "PUBKEY does not look like a public key"; exit 1;; esac

if ! id "${DEPLOY_USER}" >/dev/null 2>&1; then
  sudo adduser --disabled-password --gecos "Asset Cracker deploy" "${DEPLOY_USER}"
fi

home="$(getent passwd "${DEPLOY_USER}" | cut -d: -f6)"
sudo install -d -m 700 -o "${DEPLOY_USER}" -g "${DEPLOY_USER}" "${home}/.ssh"
printf '%s\n' "${PUBKEY}" | sudo tee "${home}/.ssh/authorized_keys" >/dev/null
sudo chown "${DEPLOY_USER}:${DEPLOY_USER}" "${home}/.ssh/authorized_keys"
sudo chmod 600 "${home}/.ssh/authorized_keys"

# Where the service binary will live: owned by acdeploy, so deploying needs no sudo at all.
sudo install -d -m 755 -o "${DEPLOY_USER}" -g "${DEPLOY_USER}" /opt/assetcracker

tmp="$(mktemp)"
cat >"${tmp}" <<SUDOERS
# Asset Cracker deploy user. Written by deploy/pi/create-deploy-user.sh.
# sudo uses the LAST matching rule, so the general rule comes first and the
# passwordless exceptions after it.

# Everything not listed below: needs acdeploy's password.
${DEPLOY_USER} ALL=(ALL) ALL

Cmnd_Alias AC_APT = /usr/bin/apt-get update, \\
    /usr/bin/apt-get install -y postgresql-common ca-certificates, \\
    /usr/bin/apt-get install -y postgresql-${PG_VERSION}, \\
    /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y

Cmnd_Alias AC_SERVICES = /usr/bin/systemctl enable --now postgresql, \\
    /usr/bin/systemctl start postgresql, /usr/bin/systemctl stop postgresql, \\
    /usr/bin/systemctl restart postgresql, /usr/bin/systemctl status postgresql, \\
    /usr/bin/systemctl start assetcracker, /usr/bin/systemctl stop assetcracker, \\
    /usr/bin/systemctl restart assetcracker, /usr/bin/systemctl status assetcracker, \\
    /usr/bin/journalctl -u assetcracker *, /usr/bin/journalctl -u postgresql *

Cmnd_Alias AC_SETUP = /usr/sbin/adduser --system --group --no-create-home --shell /usr/sbin/nologin assetcracker, \\
    /usr/bin/tee /etc/postgresql/${PG_VERSION}/main/conf.d/assetcracker.conf

${DEPLOY_USER} ALL=(root) NOPASSWD: AC_APT, AC_SERVICES, AC_SETUP
${DEPLOY_USER} ALL=(postgres) NOPASSWD: /usr/bin/psql, /usr/bin/createuser, /usr/bin/createdb
SUDOERS

# Never install a sudoers file that does not parse: a broken one can lock sudo for everyone.
sudo visudo -cf "${tmp}"
sudo install -m 440 -o root -g root "${tmp}" "/etc/sudoers.d/${DEPLOY_USER}"
rm -f "${tmp}"
sudo visudo -c >/dev/null

echo
echo "acdeploy is ready. What it may run without a password:"
sudo -l -U "${DEPLOY_USER}" | sed -n '/may run/,$p'
echo
echo "To require YOUR password for everything else, set one now on the Pi:  sudo passwd ${DEPLOY_USER}"
