import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
const local = process.argv.includes('--local');
const config = local ? { base_url: 'http://127.0.0.1:8787', api_token: 'local-test-token', mail_domain: process.env.OCPOOL_MAIL_DOMAIN || 'example.com' }
  : JSON.parse(await readFile('.state/email-bridge.json', 'utf8'));
const call = (path, options = {}) => fetch(`${config.base_url}${path}`, {
  ...options, headers: { Authorization: `Bearer ${config.api_token}`, 'Content-Type': 'application/json', ...options.headers },
  signal: AbortSignal.timeout(20000)
});
assert.equal((await fetch(`${config.base_url}/health`)).status, 200);
assert.equal((await fetch(`${config.base_url}/v1/inboxes`)).status, 401);
const email = `ocpool-smoke-${Date.now()}@${config.mail_domain}`;
const created = await call('/v1/inboxes', { method: 'POST', body: JSON.stringify({ email, ttl_seconds: 60 }) });
assert.equal(created.status, 201, await created.clone().text());
const inbox = await created.json();
try {
  if (local) {
    const raw = `From: noreply@opencode.ai\r\nTo: ${email}\r\nMessage-ID: <smoke-${Date.now()}@opencode.ai>\r\nSubject: Verify your email\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<a href="https://opencode.ai/console/verify?token=smoke&amp;next=%2F">Verify</a>`;
    const delivery = await fetch(`${config.base_url}/cdn-cgi/local/email?from=noreply%40opencode.ai&to=${encodeURIComponent(email)}`, { method: 'POST', body: raw });
    assert.equal(delivery.ok, true, await delivery.text());
  }
  const received = await call(`/v1/inboxes/${inbox.id}/messages`);
  assert.equal(received.status, 200);
  const result = await received.json();
  assert.equal(result.messages.length, local ? 1 : 0);
  if (local) assert.deepEqual(result.messages[0].urls, ['https://opencode.ai/console/verify?token=smoke&next=%2F']);
  console.log(JSON.stringify({ mode: local ? 'local-email-runtime' : 'remote-http', ok: true, message_count: result.messages.length }));
} finally {
  assert.equal((await call(`/v1/inboxes/${inbox.id}`, { method: 'DELETE' })).status, 204);
}
