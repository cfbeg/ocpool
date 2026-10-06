import test from 'node:test';
import assert from 'node:assert/strict';
import { DatabaseSync } from 'node:sqlite';
import { readFileSync } from 'node:fs';
import worker, { extractVerification } from '../src/index.js';

function fixture(t) {
  const sql = new DatabaseSync(':memory:');
  sql.exec('PRAGMA foreign_keys=ON');
  sql.exec(readFileSync(new URL('../migrations/0001_mailboxes.sql', import.meta.url), 'utf8'));
  t.after(() => sql.close());
  const prepare = query => {
    let bindings = [];
    const result = {
      bind(...values) { bindings = values; return result; },
      async first() { return sql.prepare(query).get(...bindings) ?? null; },
      async all() { return { results: sql.prepare(query).all(...bindings) }; },
      async run() { return { meta: { changes: sql.prepare(query).run(...bindings).changes } }; }
    };
    return result;
  };
  const env = { MAIL_DOMAIN: 'example.com', API_TOKEN: 'test-token', DB: {
    prepare,
    async batch(statements) {
      sql.exec('BEGIN');
      try { const result = []; for (const statement of statements) result.push(await statement.run()); sql.exec('COMMIT'); return result; }
      catch (error) { sql.exec('ROLLBACK'); throw error; }
    }
  } };
  const request = (path, method = 'GET', body, token = 'test-token') => worker.fetch(new Request(`https://worker.example${path}`, {
    method, headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    ...(body !== undefined ? { body: JSON.stringify(body) } : {})
  }), env);
  const create = async email => {
    const response = await request('/v1/inboxes', 'POST', { email });
    assert.equal(response.status, 201);
    return response.json();
  };
  const deliver = async (to, raw) => {
    let rejection;
    await worker.email({ to, from: 'noreply@opencode.ai', raw: new Blob([raw]).stream(), rawSize: Buffer.byteLength(raw),
      setReject(reason) { rejection = reason; }
    }, env);
    return rejection;
  };
  return { sql, env, request, create, deliver };
}

const message = (to, text) => `From: OpenCode <noreply@opencode.ai>\r\nTo: ${to}\r\nSubject: Verify email\r\nMessage-ID: <test@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n${text}`;

test('authentication and domain checks protect inbox creation and reads', async t => {
  const f = fixture(t);
  assert.equal((await f.request('/health', 'GET', undefined, '')).status, 200);
  assert.equal((await f.request('/v1/inboxes', 'POST', { email: 'one@example.com' }, '')).status, 401);
  assert.equal((await f.request('/v1/inboxes', 'POST', { email: 'one@elsewhere.xyz' })).status, 400);
  const inbox = await f.create('one@example.com');
  assert.equal((await f.request('/v1/inboxes', 'POST', { email: 'ONE@example.com' })).status, 409);
  assert.equal((await f.request(`/v1/inboxes/${inbox.id}/messages`, 'GET', undefined, 'wrong')).status, 401);
});

test('recipient isolation, MIME decoding, deduplication, and delete cascade', async t => {
  const f = fixture(t);
  const a = await f.create('a@example.com');
  const b = await f.create('b@example.com');
  const raw = message(a.email, '<a href="https://opencode.ai/console/verify?token=sample&amp;next=%2F">Confirm</a><p>123456</p>');
  assert.equal(await f.deliver(a.email, raw), undefined);
  await f.deliver(a.email, raw);
  const messagesA = await (await f.request(`/v1/inboxes/${a.id}/messages`)).json();
  const messagesB = await (await f.request(`/v1/inboxes/${b.id}/messages`)).json();
  assert.equal(messagesA.messages.length, 1);
  assert.deepEqual(messagesA.messages[0].urls, ['https://opencode.ai/console/verify?token=sample&next=%2F']);
  assert.deepEqual(messagesA.messages[0].codes, ['123456']);
  assert.equal(messagesB.messages.length, 0);
  assert.equal((await f.request(`/v1/inboxes/${a.id}`, 'DELETE')).status, 204);
  assert.equal(f.sql.prepare('SELECT count(*) AS n FROM messages').get().n, 0);
  assert.equal(await f.deliver(a.email, raw), 'No pending verification');
});

test('expired inboxes cannot receive or expose verification data', async t => {
  const f = fixture(t);
  const inbox = await f.create('expired@example.com');
  await f.deliver(inbox.email, message(inbox.email, '123456'));
  f.sql.prepare('UPDATE inboxes SET expires_at = 0').run();
  assert.equal((await f.request(`/v1/inboxes/${inbox.id}/messages`)).status, 404);
  assert.equal(await f.deliver(inbox.email, message(inbox.email, '654321')), 'No pending verification');
  await worker.scheduled({}, f.env);
  assert.equal(f.sql.prepare('SELECT count(*) AS n FROM messages').get().n, 0);
});

test('message counts and body size are bounded', async t => {
  const f = fixture(t);
  const inbox = await f.create('limits@example.com');
  for (let i = 0; i < 25; i++) await f.deliver(inbox.email, message(inbox.email, `Message ${i} with code 123456`));
  const result = await (await f.request(`/v1/inboxes/${inbox.id}/messages`)).json();
  assert.equal(result.messages.length, 20);
  assert.equal(await f.deliver(inbox.email, message(inbox.email, 'x'.repeat(300000))), 'Message too large');
});

test('quoted-printable soft wrapping preserves verification URLs', async t => {
  const f = fixture(t);
  const inbox = await f.create('mime@example.com');
  const raw = `From: noreply@opencode.ai\r\nTo: ${inbox.email}\r\nSubject: Your code\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nhttps://opencode.ai/console/verify?token=3Dabc=\r\ndef\r\nYour code is 987654`;
  await f.deliver(inbox.email, raw);
  const result = await (await f.request(`/v1/inboxes/${inbox.id}/messages`)).json();
  assert.deepEqual(result.messages[0].urls, ['https://opencode.ai/console/verify?token=abcdef']);
  assert.deepEqual(result.messages[0].codes, ['987654']);
});

test('non-HTTPS links and credentials in links are excluded', () => {
  assert.deepEqual(extractVerification({ text: 'http://wrong.example https://user:pass@wrong.example https://valid.example/verify?x=1' }).urls,
    ['https://valid.example/verify?x=1']);
});
