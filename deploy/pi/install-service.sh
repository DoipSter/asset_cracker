#!/usr/bin/env bash
# One-time, by an ADMIN on the Pi (not the deploy user): install the systemd unit for the
# service and a nightly database backup. Run from the Mac:
#
#   ssh bradl@rpi-v5-1.local 'bash -s' < ~/workspace/git/asset_cracker/deploy/pi/install-service.sh
#
# It does not start the service. deploy/pi/deploy.sh starts it once a release is in place;
# the deploy user is already allowed to start, stop, restart and read the logs of this unit.
# Safe to repeat: it rewrites the files it owns and nothing else.
set -euo pipefail
[ "$(id -u)" -ne 0 ] || { echo "run as your normal admin user; it calls sudo itself"; exit 1; }

sudo install -d -m 750 -o root -g assetcracker /etc/assetcracker
if [ ! -f /etc/assetcracker/env ]; then
  sudo tee /etc/assetcracker/env >/dev/null <<'ENV'
# Settings for assetcracker.service. Readable by root and the service only.
# When real trading arrives, exchange keys go HERE and nowhere else: never in git, never in
# chat, never in Agora. An admin puts them in by hand.
AC_DATABASE_URL=postgres:///assetcracker?host=/var/run/postgresql
AC_HTTP_ADDR=127.0.0.1:8377
ENV
fi
sudo chown root:assetcracker /etc/assetcracker/env
sudo chmod 640 /etc/assetcracker/env

sudo tee /etc/systemd/system/assetcracker.service >/dev/null <<'UNIT'
[Unit]
Description=Asset Cracker service
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
User=assetcracker
Group=assetcracker
EnvironmentFile=/etc/assetcracker/env
ExecStart=/opt/assetcracker/current/assetcracker
Restart=on-failure
RestartSec=5
# It needs the network and the Postgres socket, and nothing else on this machine.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
LockPersonality=yes
MemoryMax=2G

[Install]
WantedBy=multi-user.target
UNIT

# Nightly logical backup, kept on the NVMe for 14 days. This protects against mistakes, not
# against losing the disk: copying /var/backups/assetcracker off the Pi is still to be set up.
sudo install -d -m 750 -o postgres -g postgres /var/backups/assetcracker
sudo tee /usr/local/sbin/assetcracker-backup >/dev/null <<'BACKUP'
#!/usr/bin/env bash
set -euo pipefail
out="/var/backups/assetcracker/assetcracker_$(date -u +%Y%m%dT%H%M%SZ).dump"
pg_dump --format=custom --file="${out}.part" assetcracker
mv "${out}.part" "${out}"
find /var/backups/assetcracker -name 'assetcracker_*.dump' -mtime +14 -delete
BACKUP
sudo chmod 755 /usr/local/sbin/assetcracker-backup
sudo tee /etc/systemd/system/assetcracker-backup.service >/dev/null <<'UNIT'
[Unit]
Description=Asset Cracker database backup
After=postgresql.service

[Service]
Type=oneshot
User=postgres
ExecStart=/usr/local/sbin/assetcracker-backup
UNIT
sudo tee /etc/systemd/system/assetcracker-backup.timer >/dev/null <<'UNIT'
[Unit]
Description=Nightly Asset Cracker database backup

[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true

[Install]
WantedBy=timers.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable assetcracker.service assetcracker-backup.timer
sudo systemctl start assetcracker-backup.timer
echo
systemctl is-enabled assetcracker.service assetcracker-backup.timer
systemctl list-timers assetcracker-backup.timer --no-pager | sed -n '1,2p'
echo "Installed. The service is enabled but not started; deploy/pi/deploy.sh prod starts it."
