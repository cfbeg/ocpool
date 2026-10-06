#!/bin/sh
set -eu

mkdir -p /run/dbus
dbus-daemon --system --fork --nopidfile
warp-svc > /tmp/warp-svc.log 2>&1 &
warp_pid=$!
relay_pid=
cleanup() {
    if [ -n "$relay_pid" ]; then kill -TERM "$relay_pid" 2>/dev/null || true; wait "$relay_pid" 2>/dev/null || true; fi
    kill -TERM "$warp_pid" 2>/dev/null || true
}
trap cleanup TERM INT EXIT

ready=false
for attempt in $(seq 1 60); do
    if warp-cli --accept-tos status >/dev/null 2>&1; then ready=true; break; fi
    if ! kill -0 "$warp_pid" 2>/dev/null; then echo 'warp-svc exited before initialization' >&2; exit 1; fi
    sleep 1
done
if [ "$ready" != true ]; then echo 'warp-svc initialization timed out' >&2; exit 1; fi

# The named volume keeps registration stable across restarts. Only the first run
# registers a client; reconnect never deletes registration or creates accounts.
if ! warp-cli --accept-tos registration show >/dev/null 2>&1; then
    warp-cli --accept-tos registration new >/dev/null
fi
# Both inference and health use this local SOCKS listener. No transport can fall
# back to the ordinary container connection when the tunnel is disconnected.
warp-cli --accept-tos mode proxy >/dev/null
warp-cli --accept-tos proxy port 40000 >/dev/null
warp-cli --accept-tos connect >/dev/null
ocpool-egress &
relay_pid=$!
wait "$relay_pid"
