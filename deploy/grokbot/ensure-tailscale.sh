#!/bin/sh
# Starts tailscaled on a machine without systemd (for example a Grok Bot cloud box) if it is not running.
# Install the static binaries from https://pkgs.tailscale.com/stable/ into /usr/local/bin first, then
# `sudo tailscale up --hostname=HOST` once. Copy this to ~/.local/bin/ensure-tailscale and call it from ~/.bashrc,
# before the ensure-aivoicebus-forward line, so the mesh is reachable when the forwarder starts.
pgrep -x tailscaled >/dev/null && exit 0
sudo mkdir -p /var/run/tailscale
sudo setsid nohup /usr/local/bin/tailscaled \
	--state=/var/lib/tailscale/tailscaled.state \
	--socket=/var/run/tailscale/tailscaled.sock >/tmp/tailscaled.log 2>&1 </dev/null &
