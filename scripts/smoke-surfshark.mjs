import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
const key = (await readFile('.state/server/admin-key.txt', 'utf8')).trim();
const base = 'http://127.0.0.1:10000';
const headers = { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' };
const call = async (path, body) => {
  const response = await fetch(base + path, { method: body === undefined ? 'GET' : 'POST', headers,
    ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: AbortSignal.timeout(120000) });
  const text = await response.text(); assert.equal(response.status, 200, text.slice(0, 1000));
  return JSON.parse(text);
};
const state = await call('/api/egress');
const poolMode = state.config.mode === 'sticky';
assert.equal(state.config.allow_direct_fallback, false);
const exits = state.config.endpoints.filter(x => x.enabled && x.kind === 'vpn');
assert.equal(exits.length, poolMode ? 1 : 3);
const health = [];
for (const exit of exits) {
  const status = await call(`/api/egress/${exit.id}/probe`, {});
  assert(status.healthy && status.connected); assert.equal(status.backend, 'wireguard'); assert.equal(status.ip_scope, 'opencode.ai');
  assert(status.latest_handshake > Date.now() / 1000 - 180);
  if (status.pooled) {
    const ready = status.slots.filter(slot => slot.healthy && slot.connected);
    assert(ready.length >= 2, 'pooled VPN needs at least two ready devices');
    assert.equal(new Set(ready.map(slot => slot.public_ip)).size, ready.length, 'ready devices must have distinct measured exit IPs');
    assert(status.candidate_count >= status.pool_size);
  }
  health.push({ id: exit.id, public_ip: status.public_ip, server: status.vpn_server, endpoint: status.vpn_endpoint, handshake: status.latest_handshake,
    ...(status.pooled ? { pooled: true, pool_size: status.pool_size, ready_count: status.ready_count, candidate_count: status.candidate_count, slots: status.slots } : {}),
  });
}
if (!poolMode) assert.equal(new Set(health.map(x => x.public_ip)).size, 3);
const outputs = [];
const metricsOnly = process.argv.includes('--metrics-only');
for (let index = 0; index < (metricsOnly ? 0 : poolMode ? 1 : 3); index++) {
  const reply = await call('/v1/chat/completions', { model: 'opencode/muse-spark-1.3-contributor-free', messages: [{ role: 'user', content: 'Reply with exactly SURFSHARK_POOL_OK.' }], max_tokens: 256 });
  assert.match(reply.choices[0].message.content, /SURFSHARK_POOL_OK/); outputs.push(reply.choices[0].message.content);
  console.log(`native generation ${index + 1}: OK`);
}
let after, routes;
const deadline = Date.now() + 10000;
do {
  after = await call('/api/egress'); routes = after.routes.filter(x => exits.some(exit => exit.id === x.id));
  if (routes.every(x => x.active === 0)) break;
  await new Promise(resolve => setTimeout(resolve, 200));
} while (Date.now() < deadline);
if (!metricsOnly) {
  assert(routes.every(x => x.requests > 0 && x.successes > 0), 'not every VPN exit served a successful native upstream request');
}
// Other API clients and native title jobs may still hold leases. Their safety
// is verified by the transport cancellation/drain tests, not global quiescence.
const result = { tested_at: new Date().toISOString(), mode: after.config.mode, health, outputs, native_200_events: after.events.filter(x => x.kind === 'request' && x.status === 200 && x.model === 'muse-spark-1.3-contributor-free').slice(0, 12),
  routes: routes.map(x => ({ id: x.id, public_ip: x.public_ip, requests: x.requests, successes: x.successes, failures: x.failures, active: x.active })),
};
await writeFile('.state/server/surfshark-live-result.json', JSON.stringify(result, null, 2)); console.log(JSON.stringify(result, null, 2));
