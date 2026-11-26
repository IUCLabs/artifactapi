#!/usr/bin/env bash
# Daemonless ui/Dockerfile.ui: append the built ui/dist and rendered nginx conf
# onto the nginx base. Usage: scripts/ui-image.sh <repo:tag>
set -euo pipefail

# Keep in sync with the FROM in ui/Dockerfile.ui.
base=nginx:alpine@sha256:fb4658584bd9a916ffb6f6e365f97c25cfdb2923801f551ea08356e695f14ff7
base_path=${BASE_PATH:-/ui}
prefix=${base_path%/}
source_url=https://github.com/IUCLabs/artifactapi
image=$1

root=$(mktemp -d)
mkdir -p "$root/usr/share/nginx/html" "$root/etc/nginx/conf.d"
cp -r ui/dist/. "$root/usr/share/nginx/html/"
sed "s|\${BASE_PATH}|${prefix}|g" ui/nginx.conf >"$root/etc/nginx/conf.d/default.conf"
tar -C "$root" --owner=0 --group=0 --numeric-owner -cf "$root.tar" usr etc

crane mutate --platform linux/amd64 "$base" --append "$root.tar" \
  --label "org.opencontainers.image.source=$source_url" -t "$image"
