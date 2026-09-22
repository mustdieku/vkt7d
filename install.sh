#!/bin/bash
set -euo pipefail

PREFIX=/usr/local/bin
CONF=/etc/vkt7d
BIN="$PREFIX/vkt7d"

if [ "$(id -u)" -ne 0 ]; then echo "run as root" >&2; exit 1; fi

install -d -m 0755 "$CONF"
install -m 0755 vkt7d "$BIN"
install -m 0644 systemd/vkt7d.service /etc/systemd/system/vkt7d.service
if [ ! -f "$CONF/vkt7d.env" ]; then install -m 0600 systemd/vkt7d.env.example "$CONF/vkt7d.env"; fi

systemctl daemon-reload
echo "Installed $BIN and systemd unit. Edit $CONF/vkt7d.env, then:"
echo "  systemctl enable --now vkt7d"
