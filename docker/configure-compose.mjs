import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

const root = process.env.OCPool_ROOT || '/app';
const state = join(root, '.state');
const configPath = join(state, 'server', 'egress.json');
const relayURL = process.env.OCPool_RELAY_URL || 'http://surfshark-pool:18071';
const tokenPath = process.env.OCPool_RELAY_TOKEN_FILE || join(state, 'surfshark', 'relay-pool-token.txt');
const read = async path => { try { return await readFile(path, 'utf8'); } catch (error) { if (error.code === 'ENOENT') return ''; throw error; } };

const token = (await read(tokenPath)).trim();
if (token && !/^[a-f0-9]{64}$/.test(token)) throw new Error('invalid Surfshark relay token');
await mkdir(dirname(configPath), { recursive: true, mode: 0o700 });
let config;
try { config = JSON.parse(await readFile(configPath, 'utf8')); }
catch (error) {
  if (error.code !== 'ENOENT') throw error;
  if (!token) throw new Error('Surfshark relay token is missing: ' + tokenPath);
  config = { mode: 'sticky', allow_direct_fallback: false, max_attempts: 1, cooldown_seconds: 60, endpoints: [] };
}
if (!Array.isArray(config.endpoints)) config.endpoints = [];
let endpoint = config.endpoints.find(item => item?.id === 'surfshark-pool' || item?.kind === 'vpn');
if (!endpoint) {
  if (!token) throw new Error('cannot bootstrap Surfshark egress without relay token');
  endpoint = { id: 'surfshark-pool', name: 'Surfshark WireGuard pool', kind: 'vpn', enabled: true, rotate_on_limit: false, url: relayURL, secret: token };
  config.endpoints.push(endpoint);
}
if (endpoint.kind === 'vpn' || endpoint.id === 'surfshark-pool') {
  endpoint.kind = 'vpn'; endpoint.url = relayURL; endpoint.enabled = true; endpoint.rotate_on_limit = false;
  if (token) endpoint.secret = token;
  config.mode = 'sticky'; config.allow_direct_fallback = false; config.max_attempts = 1;
}
const raw = JSON.stringify(config, null, 2) + '\n';
const temporary = configPath + '.' + process.pid + '.tmp';
await writeFile(temporary, raw, { mode: 0o600 });
await rename(temporary, configPath);
