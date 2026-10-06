import PostalMime from 'postal-mime';

const MAX_MAIL_BYTES = 256 * 1024;
const MAX_MESSAGES = 20;
const noStore = { 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' };
const json = (value, status = 200) => Response.json(value, { status, headers: noStore });
const now = () => Math.floor(Date.now() / 1000);

export function normalizeEmail(value, domain) {
  if (typeof value !== 'string' || value.length > 254) return null;
  const email = value.trim().toLowerCase();
  const split = email.lastIndexOf('@');
  if (split < 1 || email.slice(split + 1) !== domain.toLowerCase()) return null;
  if (!/^[a-z0-9][a-z0-9._+-]{0,63}$/.test(email.slice(0, split))) return null;
  return email;
}

function decodeEntities(text) {
  return text.replace(/&(?:amp|quot|apos|lt|gt|#\d+|#x[\da-f]+);/gi, entity => {
    const named = { '&amp;': '&', '&quot;': '"', '&apos;': "'", '&lt;': '<', '&gt;': '>' };
    if (named[entity.toLowerCase()]) return named[entity.toLowerCase()];
    const code = entity.slice(0, 3).toLowerCase() === '&#x'
      ? parseInt(entity.slice(3, -1), 16) : parseInt(entity.slice(2, -1), 10);
    return code > 0 && code <= 0x10ffff ? String.fromCodePoint(code) : '';
  });
}

export function extractVerification(mail) {
  const source = decodeEntities(`${mail.text || ''}\n${mail.html || ''}`);
  const urls = new Set();
  for (const match of source.matchAll(/https:\/\/[^\s<>"']+/gi)) {
    const candidate = match[0].replace(/[),.;]+$/, '');
    if (candidate.length > 4096) continue;
    try {
      const url = new URL(candidate);
      if (url.protocol === 'https:' && !url.username && !url.password) urls.add(url.href);
    } catch { /* Ignore malformed link text. */ }
    if (urls.size >= 30) break;
  }
  const plain = `${mail.subject || ''}\n${mail.text || ''}\n${(mail.html || '').replace(/<[^>]*>/g, ' ')}`;
  const codes = [...new Set(plain.match(/(?<!\d)\d{6}(?!\d)/g) || [])].slice(0, 10);
  return { urls: [...urls], codes };
}

async function boundedRead(stream, limit) {
  const reader = stream.getReader();
  const chunks = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > limit) { await reader.cancel(); throw new Error('body_too_large'); }
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  const result = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength; }
  return result;
}

async function digest(value) {
  const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value));
  return [...new Uint8Array(bytes)].map(v => v.toString(16).padStart(2, '0')).join('');
}

async function cleanup(env) {
  await env.DB.prepare('DELETE FROM inboxes WHERE expires_at <= ?').bind(now()).run();
}

async function fetchHandler(request, env) {
  const url = new URL(request.url);
  if (url.pathname === '/health' && request.method === 'GET') return json({ ok: true, service: 'ocpool-email-bridge' });
  if (!env.API_TOKEN) return json({ error: 'not_configured' }, 503);
  if (request.headers.get('Authorization') !== `Bearer ${env.API_TOKEN}`) return json({ error: 'unauthorized' }, 401);
  if (url.pathname === '/v1/inboxes' && request.method === 'POST') {
    let body;
    try {
      if (!request.body) throw new Error('missing_body');
      body = JSON.parse(new TextDecoder().decode(await boundedRead(request.body, 4096)));
    } catch { return json({ error: 'invalid_json_or_size' }, 400); }
    const email = normalizeEmail(body?.email, env.MAIL_DOMAIN);
    const ttl = body?.ttl_seconds ?? 900;
    if (!email || !Number.isInteger(ttl) || ttl < 60 || ttl > 3600) return json({ error: 'invalid_email_or_ttl' }, 400);
    const timestamp = now();
    const id = crypto.randomUUID();
    const result = await env.DB.batch([
      env.DB.prepare('DELETE FROM inboxes WHERE email = ? AND expires_at <= ?').bind(email, timestamp),
      env.DB.prepare('INSERT OR IGNORE INTO inboxes (id,email,created_at,expires_at) VALUES (?,?,?,?)')
        .bind(id, email, timestamp, timestamp + ttl)
    ]);
    if (!result[1].meta.changes) return json({ error: 'email_already_pending' }, 409);
    return json({ id, email, created_at: timestamp, expires_at: timestamp + ttl }, 201);
  }
  const path = /^\/v1\/inboxes\/([a-f0-9-]{36})(\/messages)?$/.exec(url.pathname);
  if (!path) return json({ error: 'not_found' }, 404);
  const inbox = await env.DB.prepare('SELECT id,email,created_at,expires_at FROM inboxes WHERE id = ? AND expires_at > ?')
    .bind(path[1], now()).first();
  if (!inbox) return json({ error: 'not_found_or_expired' }, 404);
  if (!path[2] && request.method === 'DELETE') {
    await env.DB.prepare('DELETE FROM inboxes WHERE id = ?').bind(inbox.id).run();
    return new Response(null, { status: 204, headers: noStore });
  }
  if (path[2] && request.method === 'GET') {
    const result = await env.DB.prepare('SELECT * FROM messages WHERE inbox_id = ? ORDER BY received_at,id LIMIT 20')
      .bind(inbox.id).all();
    return json({ inbox, messages: result.results.map(row => ({
      id: row.id, received_at: row.received_at, sender: row.sender, subject: row.subject,
      urls: JSON.parse(row.urls_json), codes: JSON.parse(row.codes_json)
    })) });
  }
  return json({ error: 'method_not_allowed' }, 405);
}

export default {
  async fetch(request, env) {
    try { return await fetchHandler(request, env); }
    catch { return json({ error: 'storage_unavailable' }, 503); }
  },
  async email(message, env) {
    const email = normalizeEmail(message.to, env.MAIL_DOMAIN);
    if (!email) { message.setReject('Recipient not configured'); return; }
    if (message.rawSize > MAX_MAIL_BYTES) { message.setReject('Message too large'); return; }
    const inbox = await env.DB.prepare('SELECT id FROM inboxes WHERE email = ? AND expires_at > ?').bind(email, now()).first();
    if (!inbox) { message.setReject('No pending verification'); return; }
    let raw;
    try { raw = await boundedRead(message.raw, MAX_MAIL_BYTES); }
    catch { message.setReject('Message too large'); return; }
    let mail;
    try { mail = await PostalMime.parse(raw); }
    catch { message.setReject('Invalid MIME message'); return; }
    const verification = extractVerification(mail);
    const id = await digest(`${inbox.id}\n${new TextDecoder().decode(raw)}`);
    await env.DB.prepare(`INSERT OR IGNORE INTO messages (id,inbox_id,received_at,sender,subject,urls_json,codes_json)
      SELECT ?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM inboxes WHERE id = ? AND expires_at > ?)
      AND (SELECT count(*) FROM messages WHERE inbox_id = ?) < ?`)
      .bind(id, inbox.id, now(), (mail.from?.address || message.from).slice(0, 254),
        (mail.subject || '').slice(0, 500), JSON.stringify(verification.urls), JSON.stringify(verification.codes),
        inbox.id, now(), inbox.id, MAX_MESSAGES).run();
  },
  async scheduled(_event, env) { await cleanup(env); }
};
