# WireGuard relay backend

`ocpool-egress` runs inside one Linux Docker container with `NET_ADMIN` and the
kernel WireGuard driver. Publish its management port `18071` only on host
loopback. The relay authenticates control requests, permits only the configured
OpenCode inference origin and paths, forwards the original request headers and
body, and keeps each streaming response's tunnel lease until EOF or cancellation.

## Pool mode

Set `OCPOOL_VPN_BACKEND=wireguard` and `OCPOOL_WG_POOL_SIZE=4` to maintain four
independent WireGuard connections in the same container. Pool size is bounded to
2..32; the setup CLI accepts `--connections` and defaults to four. The complete
validated catalog is available to the scheduler. A catalog containing 280 peers
does not create 280 simultaneous interfaces: only the configured number remain
connected, while every catalog peer may be selected as a replacement.

Each slot has its own `wg0`, `wg1`, etc., configuration file, routing table, and
HTTP transport. Inference TCP and DNS sockets use Linux `SO_BINDTODEVICE` for that
slot. Policy routing selects its WireGuard table and prohibits fallback to the
ordinary container route if the tunnel goes down. Relay management replies and
encrypted WireGuard UDP retain the ordinary Docker route. Host VPN clients and
host routing are unchanged.

Setup mounts these paths:

- `/run/secrets/wg.conf`: operator base configuration, read-only.
- `/run/servers.json`: public peer catalog, read-only.
- `/var/lib/ocpool`: private writable pool state directory.

`OCPOOL_VPN_STATE_FILE=/var/lib/ocpool/limits.json` persists IP/model cooldowns
across container replacement. The default host directory is
`.state/surfshark/pool-state`. Tokens remain in a private env file or
`OCPOOL_RELAY_TOKEN_FILE`; they are never passed as values in Docker argv.

The catalog is an array of `{id, endpoint, public_key, hostname?, dns?}`. Each
endpoint uses its own verified WireGuard peer key. Duplicate endpoints and
invalid peers are excluded. The supplied private key and optional preshared key
are retained and never logged. Runtime files use mode `0600` and a controlled
template; supplied shell hooks and `SaveConfig` do not enter that template.

Each connection must pass a recent peer handshake and an actual
`https://opencode.ai/cdn-cgi/trace` request before serving inference. Generic
Cloudflare trace fallback cannot claim this backend healthy. ICMP is only a
candidate signal: silence does not prove that a peer is offline.

The scheduler selects a usable slot, records the measured public IP, and leases
that slot for the complete upstream response. IP limits, ambiguous model limits,
and account quotas remain distinct. `Retry-After` deadlines are preserved and
persisted; reconnecting to the same public IP never clears its cooldown. Cooled,
failed, and duplicate IPs are excluded. Peer replacement waits for that slot's
streams while the other healthy slots continue serving. Started SSE responses
are never replayed.

## Management

Authenticated `GET /health` retains the legacy representative fields:
`backend:wireguard`, `healthy`, `connected`, `public_ip`, `ip_scope`,
`vpn_server`, `vpn_endpoint`, and `latest_handshake`. Pool mode also returns:

- `pooled:true`, `candidate_count`, `pool_size`, `ready_count`, `cooling_count`.
- `active_flows`, the sum across all slots.
- `slots`, with each interface's health, measured IP, peer, handshake, leases,
  rotation status, cooldown deadline, and last error.

The top-level IP represents one usable slot; it is not the only IP in the pool.
Setup requires at least two verified healthy slots with distinct target IPs and
the requested `pool_size` before configuring OCPool.

Authenticated `POST /rotate` supports random alternate peer selection or
`{same_server:true,max_attempts:1}`. Same-server reconnect may keep the same IP;
`changed:false` retains its prior cooldown. `POST /drain` pauses new traffic and
waits for active flows before a container replacement. A drain timeout preserves
existing streams. `POST /resume` releases an explicit drain.

OCPool uses one logical `vpn` endpoint in `sticky` mode, disables direct fallback,
sets outer `max_attempts:1`, and sets `rotate_on_limit:false`. Docker owns slot
scheduling, cooldowns, and automatic replacement; this avoids concurrent rotation
by the application-level VPN maintenance loop. Manual management remains available.

## Single-tunnel compatibility

Without `OCPOOL_WG_POOL_SIZE>=2`, the original single WireGuard tunnel backend
remains available. `OCPOOL_VPN_SERVER_ID` selects an exact catalog entry and
`OCPOOL_WG_INITIAL_ENDPOINT` is an endpoint hint. The explicit setup `--fleet`
option retains the older numbered-container layout for migration.

Previous live tests on 2026-10-06 used three separate relays and measured Paris
`85.204.70.104`, Jakarta `93.185.162.128`, and Valencia `193.19.207.87` as target
IPs. Muse generation succeeded on those routes. Paris same-server reconnect kept
its IP and returned `changed:false`; taking its interface down produced503 rather
than ordinary-network traffic. These results are historical validation of the
single-tunnel backend, not proof of the new simultaneous pool deployment.

References: [WireGuard quick start](https://www.wireguard.com/quickstart/),
[wg-quick manual](https://git.zx2c4.com/wireguard-tools/about/src/man/wg-quick.8),
[Linux socket device binding](https://man7.org/linux/man-pages/man7/socket.7.html),
[policy rules](https://man7.org/linux/man-pages/man8/ip-rule.8.html).
