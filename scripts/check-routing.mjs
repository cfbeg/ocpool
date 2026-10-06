import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';
// Let Wrangler refresh its own OAuth credentials before reading the short-lived token.
if (!process.env.CLOUDFLARE_API_TOKEN) {
  await promisify(execFile)(process.execPath, [fileURLToPath(new URL('../node_modules/wrangler/bin/wrangler.js', import.meta.url)), 'whoami'], {
    windowsHide: true, timeout: 30000, maxBuffer: 1024 * 1024
  });
}
const configPath = join(process.env.APPDATA, 'xdg.config', '.wrangler', 'config', 'default.toml');
const config = await readFile(configPath, 'utf8');
const token = process.env.CLOUDFLARE_API_TOKEN || /^oauth_token\s*=\s*"([^"\r\n]+)"/m.exec(config)?.[1];
if (!token) throw new Error('Wrangler authentication is unavailable');
async function get(path) {
  const response = await fetch(`https://api.cloudflare.com/client/v4${path}`, {
    headers: { Authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(20000)
  });
  const body = await response.json();
  if (!body.success) throw new Error(`Cloudflare read failed (${response.status}): ${body.errors?.map(error => error.message).join('; ')}`);
  return body.result;
}
let domain = process.env.OCPOOL_MAIL_DOMAIN;
if (!domain) domain = JSON.parse(await readFile('.state/email-bridge.json', 'utf8')).mail_domain;
if (typeof domain !== 'string' || !/^[a-z0-9.-]+$/i.test(domain)) throw new Error('A valid mail domain is required');
const zones = await get('/zones?name=' + encodeURIComponent(domain));
if (zones.length !== 1) throw new Error('Expected one matching zone');
const rule = await get(`/zones/${zones[0].id}/email/routing/rules/catch_all`);
console.log(JSON.stringify({ domain, enabled: rule.enabled, actions: rule.actions }, null, 2));
