# Dedicated Docker WARP

`powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/setup-warp.ps1 -Rebuild`
builds the Go relay for Linux, builds the Ubuntu image using Cloudflare's official
APT repository, and starts `ocpool-warp-1`. The service is published only at
`http://127.0.0.1:18071`. Registration persists in the `ocpool-warp-state` named
volume. No host WARP settings or host routes are changed.

The container's WARP client runs in proxy mode. Both the Go relay and the
Cloudflare trace check use its local SOCKS5 listener; they cannot silently fall
back to an ordinary outbound connection if WARP disconnects. The image contains
the official client and needs `NET_ADMIN` and `/dev/net/tun` in the Linux engine.

Control credentials are generated in `.state/warp/control-token.txt`, with a
private Docker env file alongside it. Setup restricts that directory to the
Windows operator and SYSTEM. OCPool loads the token from disk. Do not place it in
browser JavaScript or commit it. The relay never logs request bodies or auth
headers.

## Contract

Every endpoint requires `X-Relay-Auth`.

- `GET /health` returns `healthy`, `connected`, `public_ip`, `ip_scope`, `warp` (trace value
  `on`, `plus`, or `off`), `active_flows`, `rotating`, `rotations`, and
  `rotation_attempts`, `registration_renewals`, and
  `supports_registration_renewal`. Healthy means a trace request through the
  actual proxy transport verified WARP and a public IP. The preferred check is
  `https://opencode.ai/cdn-cgi/trace`, labeled `ip_scope: opencode.ai`; if that is
  unavailable, a generic Cloudflare trace is explicitly labeled
  `ip_scope: cloudflare.com`.
- `POST /fetch` requires `X-Relay-Target: https://opencode.ai` and
  `X-Relay-Path`. Console OAuth inference permits
  `/inference/openai/v1/{chat/completions,responses,models}`,
  `/inference/anthropic/v1/messages`, and the Google
  `/inference/google/v1beta/models` catalog and individual models. Google POST
  operations are limited to `generateContent`, `streamGenerateContent`, and
  `countTokens`; only streaming may include the exact query `alt=sse`.
  `/zen/v1/{chat/completions,responses,messages,models}` is also supported.
  Inference uses POST; models uses GET. Optional `X-Relay-Method` must agree. The relay preserves
  bodies, Authorization and OpenCode headers, upstream status and Retry-After,
  and flushes SSE chunks as they arrive. It strips control/hop headers and
  does not follow redirects. No model tools are executed by this process.
- `POST /rotate` stops admitting new streams and waits up to 30 seconds for
  existing bodies to finish. If they do not drain, it returns 409 and preserves
  the current tunnel. After draining, it disconnects/reconnects the existing
  WARP registration, verifies the new connection via trace, and returns
  `{changed,old_ip,new_ip,health}`. New fetches during rotation return 503 with
  Retry-After. Concurrent rotations return 409.

An explicit rotation body `{"renew_registration":true,"max_attempts":2}`
replaces only this container's WARP registration after draining streams. Each
attempt runs disconnect, registration delete/new, proxy mode/port setup, and
connect. The maximum is two fresh registrations; the default is one, and it
stops as soon as an IP change is verified or an error occurs. An IP change is
only verified by comparing the same trace scope. A fallback to another trace
host cannot claim a target IP changed. Empty-body rotation keeps registration
and performs one reconnect.

`scripts/inspect-warp.ps1` prints current status without the control token;
`scripts/inspect-warp.ps1 -Rotate` runs the drain/reconnect operation;
`scripts/inspect-warp.ps1 -RenewRegistration -MaxAttempts 2` runs bounded
registration renewal. Docker
logs report startup; daemon diagnostics are in `/tmp/warp-svc.log` inside the
container. Rebuilding waits for active connections through Docker's graceful
stop period before replacing the container, and leaves registration intact.

Reconnecting or replacing registration does **not** guarantee a different egress IP. `changed: false` is
an ordinary, observable outcome; only verified changes increment `rotations`.
The preferred target trace describes OpenCode's observed egress. A destination's own
limit may also be account/model based, and a changed trace IP does not prove that
the destination's quota was cleared. OCPool should retain Retry-After/cooldowns
and a bounded retry budget independently of relay rotation.

In the local validation on 2026-10-05, reconnect and exactly two fresh
registrations all kept the target/generic trace IP `104.28.238.115`, with
`warp=on`. The service therefore reported `changed:false`. A separate
`ifconfig.me` request observed `104.28.206.119` through the same SOCKS transport,
which demonstrates why an arbitrary IP-check destination should not be used as
proof that the Zen quota's source IP changed.

One bounded WireGuard experiment also connected successfully and retained the
same OpenCode target IP. MASQUE was restored and verified healthy. The target
trace's Cloudflare location changed from PHX to PDX on restoration while the
public IP stayed the same; a different Cloudflare location alone is therefore
not evidence of a changed source IP.

References:

- [Official Linux setup and trace verification](https://developers.cloudflare.com/warp-client/get-started/linux/)
- [Official APT repository and supported distributions](https://pkg.cloudflareclient.com/)
