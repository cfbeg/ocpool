import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { access } from 'node:fs/promises';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { setTimeout as delay } from 'node:timers/promises';
import { prepareProfile } from './opencode-runtime.mjs';

const server = 'https://opencode.ai/console';
const clientID = 'opencode-cli';
const execute = promisify(execFile);

async function jsonRequest(path, { body, token, fetcher = fetch } = {}) {
  const response = await fetcher(server + path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { Accept: 'application/json', ...(body === undefined ? {} : { 'Content-Type': 'application/json' }), ...(token ? { Authorization: `Bearer ${token}` } : {}) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: AbortSignal.timeout(30000)
  });
  const value = await response.json();
  if (!response.ok && !['authorization_pending','slow_down'].includes(value.error)) {
    throw new Error(`OpenCode OAuth HTTP ${response.status}: ${typeof value.error === 'string' ? value.error : 'request_failed'}`);
  }
  return value;
}

export function validateDevice(device) {
  const url = new URL(device.verification_uri_complete, server + '/');
  if (url.origin !== 'https://opencode.ai' || url.pathname !== '/console/device') throw new Error('unexpected_device_authority');
  const normalize = value => String(value || '').replaceAll('-', '').toUpperCase();
  if (!device.device_code || !device.user_code || normalize(url.searchParams.get('user_code')) !== normalize(device.user_code)) throw new Error('device_code_mismatch');
  if (!Number.isFinite(device.expires_in) || device.expires_in <= 0) throw new Error('invalid_device_expiry');
  return url.href;
}

// This schema is the Console account repository used by the pinned OpenCode CLI.
// Validate it before starting a transaction; all values use bound parameters.
export function persistConsoleAccount(db, { user, orgID, token, expectedEmail, now = Date.now() }) {
  if (!user.id || user.email?.toLowerCase() !== expectedEmail.toLowerCase()) throw new Error('oauth_identity_mismatch');
  if (typeof token.access_token !== 'string' || !token.access_token || typeof token.refresh_token !== 'string' || !token.refresh_token || !Number.isFinite(token.expires_in) || token.expires_in <= 0) throw new Error('invalid_oauth_token_response');
  const accountColumns = new Set(db.prepare('PRAGMA table_info(account)').all().map(row => row.name));
  const stateColumns = new Set(db.prepare('PRAGMA table_info(account_state)').all().map(row => row.name));
  for (const column of ['id','email','url','access_token','refresh_token','token_expiry','time_created','time_updated']) if (!accountColumns.has(column)) throw new Error('unsupported_opencode_account_schema');
  for (const column of ['id','active_account_id','active_org_id']) if (!stateColumns.has(column)) throw new Error('unsupported_opencode_account_schema');
  db.exec('PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; BEGIN IMMEDIATE');
  try {
    const current = db.prepare('SELECT a.email FROM account_state s JOIN account a ON a.id=s.active_account_id WHERE s.id=1').get();
    if (current && current.email.toLowerCase() !== expectedEmail.toLowerCase()) throw new Error('profile_identity_mismatch');
    db.prepare(`INSERT INTO account(id,email,url,access_token,refresh_token,token_expiry,time_created,time_updated)
      VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET email=excluded.email,url=excluded.url,
      access_token=excluded.access_token,refresh_token=excluded.refresh_token,token_expiry=excluded.token_expiry,time_updated=excluded.time_updated`)
      .run(user.id,user.email,server,token.access_token,token.refresh_token,now+token.expires_in*1000,now,now);
    db.prepare('INSERT INTO account_state(id,active_account_id,active_org_id) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET active_account_id=excluded.active_account_id,active_org_id=excluded.active_org_id').run(user.id,orgID || null);
    db.exec('COMMIT');
  } catch (error) { db.exec('ROLLBACK'); throw error; }
}

export async function authorizeHeadless({ directory, page, email, report }) {
  const profile = await prepareProfile(directory);
  // A read-only CLI command initializes/migrates its own DB without opening a browser.
  await execute(profile.executable, ['db','SELECT 1','--format','json','--pure'], {
    cwd: join(profile.root,'workspace'), env: profile.env, windowsHide: true, timeout: 60000, maxBuffer: 1024*1024
  });
  const databasePath = join(profile.root,'data','opencode','opencode.db');
  await access(databasePath);
  const device = await jsonRequest('/auth/device/code', { body: { client_id: clientID } });
  const approvalURL = validateDevice(device);
  const deadline = Date.now() + device.expires_in * 1000;
  report('oauth_device_issued');
  await page.goto(approvalURL, { waitUntil: 'networkidle' });
  const sessionResponse = await page.request.get(server + '/auth/session');
  if (!sessionResponse.ok()) throw new Error('browser_session_missing');
  const session = await sessionResponse.json();
  if ((session.user || session).email?.toLowerCase() !== email.toLowerCase()) throw new Error('browser_identity_mismatch');
  const authorize = page.getByRole('button', { name:'Authorize', exact:true });
  await authorize.waitFor({ state:'visible' });
  if (!await authorize.isEnabled()) throw new Error('device_approval_not_ready');
  await authorize.click();
  await page.getByText('Device authorized', { exact:true }).waitFor({ state:'visible' });
  let interval = Math.max(1, Number(device.interval) || 5);
  let token;
  while (Date.now() < deadline) {
    await delay(interval * 1000);
    const result = await jsonRequest('/auth/device/token', { body: { grant_type:'urn:ietf:params:oauth:grant-type:device_code',device_code:device.device_code,client_id:clientID } });
    if (result.error === 'authorization_pending') continue;
    if (result.error === 'slow_down') { interval += 5; continue; }
    token = result; break;
  }
  if (!token) throw new Error('device_code_expired');
  const [user,orgs] = await Promise.all([jsonRequest('/api/user',{token:token.access_token}),jsonRequest('/api/orgs',{token:token.access_token})]);
  if (!Array.isArray(orgs)) throw new Error('invalid_oauth_organizations');
  const db = new DatabaseSync(databasePath);
  try { persistConsoleAccount(db,{user,orgID:orgs[0]?.id,token,expectedEmail:email}); }
  finally { db.close(); }
  report('oauth_saved_to_profile');
}
