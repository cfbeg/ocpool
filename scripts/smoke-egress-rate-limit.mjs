import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
const base = 'http://127.0.0.1:10000';
const key = (await readFile('.state/server/admin-key.txt', 'utf8')).trim();
const headers = { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' };
const probe = await fetch(base + '/api/egress/warp-1/probe', { method: 'POST', headers, body: '{}', signal: AbortSignal.timeout(15000) });
assert.equal(probe.status, 200);
const before = await (await fetch(base + '/api/egress', { headers })).json();
assert(before.config.mode !== 'direct' && !before.config.allow_direct_fallback, 'this check requires WARP-only mode');
const warp = before.routes.find(x => x.id === 'warp-1');
assert(warp.cooldown_until > Date.now() / 1000, 'no real rate limit is currently recorded');
const begin = Date.now();
const response = await fetch(base + '/v1/chat/completions', {
  method: 'POST', headers, body: JSON.stringify({ model: 'opencode/muse-spark-1.3-contributor-free', messages: [{ role: 'user', content: 'Reply OK.' }] }),
  signal: AbortSignal.timeout(5000),
});
const body = await response.json();
assert.equal(response.status, 429, JSON.stringify(body)); assert(response.headers.get('retry-after')); assert.equal(body.error.type, 'rate_limit_error');
const report = { tested_at: new Date().toISOString(), status: response.status, elapsed_ms: Date.now() - begin, retry_after: response.headers.get('retry-after'), public_ip: warp.public_ip, cooldown_until: warp.cooldown_until };
await writeFile('.state/server/egress-rate-limit-result.json', JSON.stringify(report, null, 2)); console.log(JSON.stringify(report));
