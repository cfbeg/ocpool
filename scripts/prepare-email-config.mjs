import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { randomBytes } from 'node:crypto';
await mkdir('.state', { recursive: true, mode: 0o700 });
let config;
try { config = JSON.parse(await readFile('.state/email-bridge.json', 'utf8')); }
catch (error) { if (error.code !== 'ENOENT') throw error; }
config ??= { api_token: randomBytes(32).toString('hex'), base_url: '', mail_domain: process.env.OCPOOL_MAIL_DOMAIN || 'example.com' };
if (process.argv[2]) config.base_url = new URL(process.argv[2]).origin;
await writeFile('.state/email-bridge.json', JSON.stringify(config, null, 2), { mode: 0o600 });
await writeFile('.state/worker-secrets.json', JSON.stringify({ API_TOKEN: config.api_token }), { mode: 0o600 });
await writeFile('email-worker/.dev.vars', 'API_TOKEN=local-test-token\n', { mode: 0o600 });
console.log('Created private bridge configuration; credentials not printed.');
