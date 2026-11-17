#!/usr/bin/env bash
# Publish the GPT Arm report page to a server over SSH.
#
#   site/deploy.sh [ssh-host]        # default: the "hetzner" alias from ~/.ssh/config
#
# Copies site/ (page, assets, data, media) and results/raw (the run logs the page links to) to
# /srv/gptarm-report, installs the page's own Caddy config and systemd unit (port 8930) the first time,
# and restarts only that unit. It never edits another service's configuration.
set -euo pipefail
HOST="${1:-${REPORT_HOST:-hetzner}}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

[ -f "$HERE/media/manifest.json" ] || { echo "build the media first: python site/build_media.py ..." >&2; exit 1; }

ssh "$HOST" 'mkdir -p /srv/gptarm-report/data/raw /etc/gptarm-report'
rsync -a --delete --exclude 'server/' --exclude 'deploy.sh' --exclude 'build_media.py' --exclude '.DS_Store' \
  --exclude 'data/raw/' "$HERE/" "$HOST:/srv/gptarm-report/"
rsync -a --delete "$ROOT/results/raw/" "$HOST:/srv/gptarm-report/data/raw/"
rsync -a "$HERE/server/Caddyfile" "$HOST:/etc/gptarm-report/Caddyfile"
rsync -a "$HERE/server/gptarm-report.service" "$HOST:/etc/systemd/system/gptarm-report.service"
ssh "$HOST" 'chmod -R a+rX /srv/gptarm-report /etc/gptarm-report && caddy validate --config /etc/gptarm-report/Caddyfile --adapter caddyfile >/dev/null \
  && systemctl daemon-reload && systemctl enable --quiet gptarm-report && systemctl restart gptarm-report && sleep 1 \
  && systemctl is-active gptarm-report && curl -s -o /dev/null -w "local check: HTTP %{http_code}\n" http://127.0.0.1:8930/'
