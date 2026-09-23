#!/usr/bin/env bash
# One-time, by an ADMIN on the Pi (not the deploy user): put the Pi on your Tailscale network and
# publish the service's page to it, so the page opens from your laptop or phone anywhere you are,
# with no SSH tunnel, and from nothing that is not yours. Run from the Mac:
#
#   ssh -t bradl@rpi-v5-1.local 'bash -s' < ~/workspace/git/asset_cracker/deploy/pi/install-tailscale.sh
#
# The service keeps listening on 127.0.0.1:8377 and nothing in it changes. `tailscale serve`
# takes that port and answers on the tailnet only, over HTTPS with a certificate Tailscale
# issues for the machine's name. The buckets page has no login of its own: the tailnet IS the
# login, exactly as the SSH tunnel was. Do not use `tailscale funnel`: that is the public internet.
#
# Safe to repeat. The first run prints a login URL; open it in your browser and approve the machine.
set -euo pipefail
[ "$(id -u)" -ne 0 ] || { echo "run as your normal admin user; it calls sudo itself"; exit 1; }

if ! command -v tailscale >/dev/null; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi
sudo systemctl enable --now tailscaled

if ! tailscale status >/dev/null 2>&1; then
  echo "Tailscale needs your login once. A URL follows: open it, sign in, approve this machine."
  sudo tailscale up --ssh=false
fi

# Publish the page to the tailnet only, in the background, surviving reboots.
sudo tailscale serve --bg 8377

echo
tailscale status | head -3
name="$(tailscale status --json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["Self"]["DNSName"].rstrip("."))')"
echo
echo "The page is at https://${name}/ from any device on your tailnet. Install Tailscale on the phone"
echo "and the laptop, sign in to the same account, and the address works there too. tools/view.sh"
echo "still works for the tunnel, if you ever want it."
