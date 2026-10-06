#!/bin/sh
set -eu
umask 077
mkdir -p /app/.state /app/.home
node /usr/local/bin/configure-compose.mjs
exec /app/bin/ocpool --root /app --listen "${OCPool_LISTEN:-0.0.0.0:10000}"
