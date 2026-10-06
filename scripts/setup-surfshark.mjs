import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { randomBytes } from 'node:crypto';
import { isIP } from 'node:net';
import { mkdir, readFile, writeFile, rename, chmod } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { shuffle, validPublicKey, publicIPv4, validHostname } from './surfshark-catalog.mjs';

const execFileAsync = promisify(execFile);
const PROJECT_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const IMAGE = 'ocpool-surfshark:local';
const OWNER = 'ocpool-surfshark-fleet';
const delay = ms => new Promise(resolveDelay => setTimeout(resolveDelay, ms));

async function systemCommand(file, args, options = {}) {
  try {
    const result = await execFileAsync(file, args, {
      cwd: options.cwd ?? PROJECT_ROOT, env: options.env ?? process.env,
      windowsHide: true, shell: false, timeout: options.timeout ?? 120_000,
      maxBuffer: 16 << 20,
    });
    return result.stdout.trim();
  } catch (error) {
    // Neither process output nor env-file contents enter a setup error.
    throw new Error((options.label ?? file) + ' failed (exit ' + (error.code ?? 'timeout') + ')');
  }
}
function ownedName(name) {
  if (name !== 'ocpool-surfshark' && !/^ocpool-surfshark-(?:[1-9]|1[0-6])$/.test(name)) throw new Error('Refusing to change an unrelated Docker container');
  return name;
}
export function validateOptions(options) {
  const count = options.count ?? 3;
  const portBase = options.portBase ?? 18100;
  const attempts = options.startupAttempts ?? 3;
  if (!Number.isInteger(count) || count < 1 || count > 16 || !Number.isInteger(portBase) || portBase < 1024 || portBase + count > 65535 || !Number.isInteger(attempts) || attempts < 1 || attempts > 3) throw new Error('Invalid fleet count, ports or bounded attempt count');
  return { ...options, count, portBase, startupAttempts: attempts };
}
export function validatePoolOptions(options = {}) {
  const port = options.port ?? 18100;
  const startupAttempts = options.startupAttempts ?? 3;
  const connections = options.connections ?? 4;
  if (!Number.isInteger(port) || port < 1024 || port > 65534 || !Number.isInteger(startupAttempts) || startupAttempts < 1 || startupAttempts > 5 || !Number.isInteger(connections) || connections < 2 || connections > 32) throw new Error('Invalid Surfshark pool port, connections or startup attempts');
  return { ...options, port, startupAttempts, connections };
}
export function candidatePool(catalog) {
  if (!Array.isArray(catalog) || catalog.length > 2048) throw new Error('Invalid Surfshark server catalog');
  const unique = new Map();
  for (const candidate of catalog) {
    const parts = candidate.endpoint?.split(':');
    if (!/^[a-zA-Z0-9_-]{1,100}$/.test(candidate.id ?? '') || parts?.length !== 2 || !publicIPv4(parts[0]) || Number(parts[1]) !== 51820 || !validPublicKey(candidate.public_key) || !validHostname(candidate.hostname)) continue;
    if (!unique.has(candidate.endpoint)) unique.set(candidate.endpoint, candidate);
  }
  const randomized = shuffle([...unique.values()]);
  return [...randomized.filter(candidate => candidate.icmp_status === 'reply'), ...randomized.filter(candidate => candidate.icmp_status !== 'reply')];
}
export function chooseCandidate(candidates, usedHosts, usedEndpoints, tried) {
  return candidates.find(candidate => !usedHosts.has(candidate.hostname) && !usedEndpoints.has(candidate.endpoint) && !tried.has(candidate.id));
}
export function verifiedHealth(health, candidate, existingIPs = new Set(), now = Date.now()) {
  const timestamp = Number(health?.latest_handshake);
  return health?.healthy === true && health?.connected === true && health.backend === 'wireguard' &&
    health.ip_scope === 'opencode.ai' && isIP(health.public_ip ?? '') !== 0 &&
    Number.isInteger(timestamp) && timestamp > 0 && timestamp <= Math.floor(now / 1000) + 5 && now / 1000 - timestamp <= 180 &&
    (!candidate || health.vpn_endpoint === candidate.endpoint) && !existingIPs.has(health.public_ip);
}
export function verifiedPoolHealth(health, { minReady = 2, expectedConnections, now = Date.now() } = {}) {
  if (!verifiedHealth(health, null, new Set(), now) || health.pooled !== true || !Array.isArray(health.slots)) return false;
  if (expectedConnections !== undefined && Number(health.pool_size) !== expectedConnections) return false;
  const ips = new Set(), endpoints = new Set();
  const ready = health.slots.filter(slot => {
    if (!verifiedHealth(slot, null, ips, now) || endpoints.has(slot.vpn_endpoint)) return false;
    ips.add(slot.public_ip); endpoints.add(slot.vpn_endpoint);
    return true;
  });
  return ready.length >= minReady;
}
export function dockerRunArgs({ name, port, envFile, baseConfig, catalogFile }) {
  ownedName(name);
  return ['run', '--detach', '--name', name, '--restart', 'unless-stopped',
    '--label', 'ocpool.owner=' + OWNER, '--label', 'ocpool.role=wireguard-egress',
    '--cap-add', 'NET_ADMIN', '--sysctl', 'net.ipv6.conf.all.disable_ipv6=0',
    '--publish', '127.0.0.1:' + port + ':18071', '--env-file', envFile,
    '--mount', 'type=bind,source=' + baseConfig + ',target=/run/secrets/wg.conf,readonly',
    '--mount', 'type=bind,source=' + catalogFile + ',target=/run/servers.json,readonly', IMAGE];
}
export function dockerPoolArgs({ envFile, baseConfig, catalogFile, stateDirectory, name = 'ocpool-surfshark', port = 18100 }) {
  if (!stateDirectory) throw new Error('Persistent pool state directory is required');
  const args = dockerRunArgs({ name, port, envFile, baseConfig, catalogFile });
  args.splice(args.length - 1, 0, '--mount', 'type=bind,source=' + stateDirectory + ',target=/var/lib/ocpool');
  return args;
}
async function atomicJSON(path, value) {
  await mkdir(dirname(path), { recursive: true });
  const temporary = path + '.' + process.pid + '.tmp';
  await writeFile(temporary, JSON.stringify(value, null, 2) + '\n', { mode: 0o600 });
  await rename(temporary, path);
}
async function protectDirectory(path, command) {
  await mkdir(path, { recursive: true, mode: 0o700 });
  if (process.platform !== 'win32') { await chmod(path, 0o700); return; }
  const sid = (await command('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', '([System.Security.Principal.WindowsIdentity]::GetCurrent()).User.Value'], { label: 'Read operator SID' })).trim();
  if (!/^S-1-[0-9-]+$/.test(sid)) throw new Error('Could not identify Windows operator');
  await command('icacls.exe', [path, '/inheritance:r', '/grant:r', '*' + sid + ':(OI)(CI)F', '*S-1-5-18:(OI)(CI)F'], { label: 'Protect Surfshark state' });
}
async function relayToken(path) {
  try {
    const token = (await readFile(path, 'utf8')).trim();
    if (!/^[a-f0-9]{64}$/.test(token)) throw new Error('Existing relay token has an invalid format');
    return token;
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    const token = randomBytes(32).toString('hex');
    try { await writeFile(path, token + '\n', { flag: 'wx', mode: 0o600 }); }
    catch (writeError) { if (writeError.code === 'EEXIST') return relayToken(path); throw writeError; }
    return token;
  }
}
async function readHealth(url, token, fetchImpl = fetch, timeoutMs = 7000) {
  const response = await fetchImpl(url + '/health', { headers: { 'X-Relay-Auth': token }, signal: AbortSignal.timeout(timeoutMs) });
  if (!response.ok) throw new Error('Relay health HTTP ' + response.status);
  return response.json();
}
async function waitHealth(url, token, candidate, ips, { fetchImpl, waitMs = 60_000 } = {}) {
  const deadline = Date.now() + waitMs;
  let duplicate = false;
  while (Date.now() < deadline) {
    try {
      const health = await readHealth(url, token, fetchImpl, Math.max(1, Math.min(7000, deadline - Date.now())));
      if (verifiedHealth(health, candidate, ips)) return health;
      if (verifiedHealth(health, candidate) && ips.has(health.public_ip)) { duplicate = true; break; }
    } catch {}
    await delay(Math.min(1000, Math.max(0, deadline - Date.now())));
  }
  throw new Error(duplicate ? 'Candidate shares an existing target egress IP' : 'Candidate did not verify a recent handshake and target trace');
}
async function waitPoolHealth(url, token, options = {}) {
  const { fetchImpl, expectedConnections, waitMs = 120_000 } = options;
  const deadline = Date.now() + waitMs;
  while (Date.now() < deadline) {
    try {
      const health = await readHealth(url, token, fetchImpl, Math.max(1, Math.min(7000, deadline - Date.now())));
      if (verifiedPoolHealth(health, { expectedConnections })) return health;
    } catch {}
    await delay(Math.min(1000, Math.max(0, deadline - Date.now())));
  }
  throw new Error('Pool did not verify two distinct healthy WireGuard exits');
}
async function containerInfo(name, command) {
  ownedName(name);
  const names = await command('docker', ['ps', '-a', '--filter', 'name=^/' + name + '$', '--format', '{{.Names}}'], { label: 'Find fleet container' });
  if (!names.split(/\r?\n/).includes(name)) return null;
  const raw = await command('docker', ['inspect', '--format', '{{json .Config.Labels}}|{{.Config.Image}}|{{.State.Status}}', name], { label: 'Inspect fleet ownership' });
  const [labelsJSON, image, status] = raw.split('|');
  const labels = JSON.parse(labelsJSON || '{}');
  if (labels?.['ocpool.owner'] !== OWNER && image !== IMAGE) throw new Error('Named container is not owned by the Surfshark fleet: ' + name);
  return { status };
}
async function stopContainer(name, url, token, command, fetchImpl, running = true, requireDrain = false) {
  ownedName(name);
  if (running) {
    let health;
    try { health = await readHealth(url, token, fetchImpl); } catch {}
    if (!health && requireDrain) throw new Error('Relay control unavailable; existing container preserved');
    if (health) {
      const drained = await fetchImpl(url + '/drain', { method: 'POST', headers: { 'X-Relay-Auth': token }, signal: AbortSignal.timeout(35_000) });
      const result = await drained.json();
      if (!drained.ok || !result.drained || result.active_flows !== 0) {
        try { await fetchImpl(url + '/resume', { method: 'POST', headers: { 'X-Relay-Auth': token }, signal: AbortSignal.timeout(7000) }); } catch {}
        throw new Error('Active relay flows did not drain; existing container preserved');
      }
    }
    await command('docker', ['stop', '--timeout', '45', name], { label: 'Stop owned fleet container' });
  }
  await command('docker', ['rm', name], { label: 'Remove owned fleet container' });
}
async function retireLegacyRelays(stateDir, command, fetchImpl) {
  const retired = [], preserved = [];
  for (let index = 1; index <= 16; index++) {
    const name = 'ocpool-surfshark-' + index;
    const existing = await containerInfo(name, command);
    if (!existing) continue;
    let token = '';
    try { token = (await readFile(join(stateDir, 'relay-' + index + '-token.txt'), 'utf8')).trim(); } catch {}
    try {
      if (/^[a-f0-9]{64}$/.test(token)) {
        await stopContainer(name, 'http://127.0.0.1:' + (18100 + index), token, command, fetchImpl, existing.status === 'running', true);
      } else {
        if (existing.status === 'running') throw new Error('Relay control token unavailable; existing container preserved');
        await command('docker', ['rm', name], { label: 'Remove legacy Surfshark relay' });
      }
      retired.push(name);
    } catch (error) { preserved.push({ name, reason: error.message }); }
  }
  return { retired, preserved };
}
async function buildImage(rootDir, stateDir, command, log) {
  const buildDir = join(stateDir, 'build');
  await mkdir(buildDir, { recursive: true, mode: 0o700 });
  log({ stage: 'build', message: 'Building Linux WireGuard relay' });
  await command('go', ['build', '-trimpath', '-o', join(buildDir, 'ocpool-egress'), './cmd/ocpool-egress'], {
    cwd: rootDir, env: { ...process.env, GOOS: 'linux', GOARCH: 'amd64', CGO_ENABLED: '0' }, label: 'Build Linux relay', timeout: 180_000,
  });
  for (const name of ['Dockerfile', 'entrypoint.sh']) await writeFile(join(buildDir, name), (await readFile(join(rootDir, 'ops/wireguard', name), 'utf8')).replaceAll('\r\n', '\n'));
  await writeFile(join(buildDir, '.dockerignore'), '*\n!Dockerfile\n!entrypoint.sh\n!ocpool-egress\n');
  log({ stage: 'image', message: 'Building dedicated Surfshark image' });
  await command('docker', ['build', '--tag', IMAGE, buildDir], { cwd: rootDir, label: 'Build Surfshark image', timeout: 600_000 });
}

export async function configureFleet(fleet, { rootDir = PROJECT_ROOT, appURL = 'http://127.0.0.1:10000', expectedCount = 3, fetchImpl = fetch, readHealthImpl = readHealth } = {}) {
  if (!fleet.length || fleet.length > 16 || fleet.length !== expectedCount) throw new Error('Ready fleet does not match the requested count');
  const app = new URL(appURL);
  if (!['http:', 'https:'].includes(app.protocol) || !['127.0.0.1', 'localhost', '[::1]'].includes(app.hostname) || app.username || app.password || app.search || app.hash || !['', '/'].includes(app.pathname)) throw new Error('OCPool management URL must be a local origin');
  appURL = app.origin;
  const ips = new Set(), endpoints = new Set(), records = [];
  for (const item of fleet) {
    const token = (await readFile(resolve(rootDir, item.token_file), 'utf8')).trim();
    const health = await readHealthImpl(item.url, token, fetchImpl);
    if (!verifiedHealth(health, null, ips) || endpoints.has(health.vpn_endpoint)) throw new Error('Fleet is not healthy with distinct target egress IPs');
    ips.add(health.public_ip); endpoints.add(health.vpn_endpoint);
    records.push({ item, token });
  }
  const admin = (await readFile(join(rootDir, '.state/server/admin-key.txt'), 'utf8')).trim();
  const headers = { Authorization: 'Bearer ' + admin, 'Content-Type': 'application/json' };
  const response = await fetchImpl(appURL + '/api/egress', { headers, signal: AbortSignal.timeout(15_000) });
  if (!response.ok) throw new Error('OCPool egress API HTTP ' + response.status);
  const current = await response.json();
  if (!current.config?.endpoints) throw new Error('OCPool egress configuration is unavailable');
  const selected = new Set(fleet.map(item => item.id));
  const endpointsToKeep = current.config.endpoints.filter(endpoint => !selected.has(endpoint.id)).map(endpoint => ({ ...endpoint, enabled: false }));
  const config = {
    ...current.config, mode: 'spread', allow_direct_fallback: false, max_attempts: 3,
    endpoints: [...records.map(({ item, token }) => ({ id: item.id, name: 'Surfshark ' + item.id.replace('surfshark-', '') + ' / ' + item.vpn_server, kind: 'vpn', url: item.url, secret: token, enabled: true, rotate_on_limit: true })), ...endpointsToKeep],
  };
  const saved = await fetchImpl(appURL + '/api/egress', { method: 'PUT', headers, body: JSON.stringify(config), signal: AbortSignal.timeout(20_000) });
  if (!saved.ok) throw new Error('OCPool rejected VPN fleet configuration (HTTP ' + saved.status + '); ready fleet preserved');
  return { mode: 'spread', enabled_count: fleet.length, allow_direct_fallback: false };
}

/** Configure one relay whose WireGuard backend owns the complete catalog. */
export async function configurePool(pool, { rootDir = PROJECT_ROOT, appURL = 'http://127.0.0.1:10000', candidateCount = 0, fetchImpl = fetch, readHealthImpl = readHealth } = {}) {
  if (!pool?.id || !pool?.url || !pool?.token_file) throw new Error('Invalid Surfshark pool record');
  const app = new URL(appURL);
  if (!['http:', 'https:'].includes(app.protocol) || !['127.0.0.1', 'localhost', '[::1]'].includes(app.hostname) || app.username || app.password || app.search || app.hash || !['', '/'].includes(app.pathname)) throw new Error('OCPool management URL must be a local origin');
  appURL = app.origin;
  const token = (await readFile(resolve(rootDir, pool.token_file), 'utf8')).trim();
  const health = await readHealthImpl(pool.url, token, fetchImpl);
  if (!verifiedPoolHealth(health, { expectedConnections: pool.connections ?? 4 })) throw new Error('Surfshark pool relay needs two distinct healthy exits');
  const admin = (await readFile(join(rootDir, '.state/server/admin-key.txt'), 'utf8')).trim();
  const headers = { Authorization: 'Bearer ' + admin, 'Content-Type': 'application/json' };
  const response = await fetchImpl(appURL + '/api/egress', { headers, signal: AbortSignal.timeout(15_000) });
  if (!response.ok) throw new Error('OCPool egress API HTTP ' + response.status);
  const current = await response.json();
  if (!current.config?.endpoints) throw new Error('OCPool egress configuration is unavailable');
  const endpoints = current.config.endpoints.filter(endpoint => endpoint.id !== pool.id).map(endpoint => ({ ...endpoint, enabled: false }));
  endpoints.unshift({ id: pool.id, name: 'Surfshark pool (' + (candidateCount || 'all') + ' endpoints)', kind: 'vpn', url: pool.url, secret: token, enabled: true, rotate_on_limit: false });
  const config = { ...current.config, mode: 'sticky', allow_direct_fallback: false, max_attempts: 1, endpoints };
  const saved = await fetchImpl(appURL + '/api/egress', { method: 'PUT', headers, body: JSON.stringify(config), signal: AbortSignal.timeout(20_000) });
  if (!saved.ok) throw new Error('OCPool rejected Surfshark pool configuration (HTTP ' + saved.status + ')');
  return { mode: 'sticky', enabled_count: 1, candidate_count: candidateCount || null, connections: pool.connections ?? 4, ready_count: health.ready_count, allow_direct_fallback: false };
}

/** Start one Docker relay with the complete validated catalog mounted read-only. */
export async function setupPool(options = {}, dependencies = {}) {
  options = validatePoolOptions(options);
  const rootDir = resolve(options.rootDir ?? PROJECT_ROOT);
  const stateDir = resolve(options.stateDir ?? join(rootDir, '.state/surfshark'));
  // 18100 is intentionally outside the legacy fleet's 18101..18116 range,
  // allowing a pool migration while the old relays are still draining.
  const { port, startupAttempts, connections } = options;
  const command = dependencies.command ?? systemCommand;
  const fetchImpl = dependencies.fetchImpl ?? fetch;
  const waitHealthImpl = dependencies.waitPoolHealth ?? (dependencies.waitHealth ? async (url, token, opts) => dependencies.waitHealth(url, token, null, new Set(), opts) : waitPoolHealth);
  const log = dependencies.log ?? (value => console.log(JSON.stringify(value)));
  const baseConfig = join(stateDir, 'base.conf'), catalogFile = join(stateDir, 'servers.json');
  const poolState = join(stateDir, 'pool-state');
  const tokenPath = join(stateDir, 'relay-pool-token.txt'), envFile = join(stateDir, 'relay-pool.env');
  const name = 'ocpool-surfshark', id = 'surfshark-pool', url = 'http://127.0.0.1:' + port;
  const result = { configured: false, ready: false, candidate_count: 0, connections, pool: null, startup_failures: [] };
  await protectDirectory(stateDir, command);
  await mkdir(poolState, { recursive: true, mode: 0o700 });
  await readFile(baseConfig);
  const candidates = candidatePool(JSON.parse(await readFile(catalogFile, 'utf8')));
  if (!candidates.length) throw new Error('No valid WireGuard candidates');
  result.candidate_count = candidates.length;
  if ((await command('docker', ['info', '--format', '{{.OSType}}'], { label: 'Check Docker engine' })).trim() !== 'linux') throw new Error('Docker Linux engine is required');
  if (options.buildImage !== false) await buildImage(rootDir, stateDir, command, log);
  const token = await relayToken(tokenPath);
  const existing = await containerInfo(name, command);
  let health;
  if (existing && !options.rebuild && existing.status === 'running') {
    try {
      health = await waitHealthImpl(url, token, { fetchImpl, expectedConnections: connections });
      if (!verifiedPoolHealth(health, { expectedConnections: connections })) throw new Error('Existing pool did not verify distinct healthy target exits');
    } catch { result.startup_failures.push({ reason: 'Existing pool did not verify healthy egress; use --rebuild to replace it' }); }
  }
  if (!health) {
    if (existing) await stopContainer(name, url, token, command, fetchImpl, existing.status === 'running', true);
    for (let attempt = 1; attempt <= startupAttempts; attempt++) {
      await writeFile(envFile, 'OCPOOL_RELAY_TOKEN=' + token + '\nOCPOOL_VPN_BACKEND=wireguard\nOCPOOL_WG_POOL_SIZE=' + connections + '\nOCPOOL_VPN_STATE_FILE=/var/lib/ocpool/limits.json\n', { mode: 0o600 });
      log({ stage: 'start', id, attempt, candidate_count: candidates.length, connections });
      try {
        await command('docker', dockerPoolArgs({ name, port, envFile, baseConfig, catalogFile, stateDirectory: poolState }), { label: 'Start Surfshark pool relay' });
        health = await waitHealthImpl(url, token, { fetchImpl, expectedConnections: connections });
        if (!verifiedPoolHealth(health, { expectedConnections: connections })) throw new Error('Pool did not verify distinct healthy target exits');
        break;
      } catch (error) {
        health = undefined;
        result.startup_failures.push({ attempt, reason: error.message });
        const failed = await containerInfo(name, command);
        if (failed) await stopContainer(name, url, token, command, fetchImpl, failed.status === 'running');
      }
    }
  }
  if (health) {
    const relativeToken = tokenPath.startsWith(rootDir + '\\') || tokenPath.startsWith(rootDir + '/') ? tokenPath.slice(rootDir.length + 1).replaceAll('\\', '/') : tokenPath;
    result.pool = { id, container: name, url, token_file: relativeToken, candidate_count: candidates.length, connections, ready_count: health.ready_count, vpn_server: health.vpn_server, vpn_endpoint: health.vpn_endpoint, public_ip: health.public_ip, ip_scope: health.ip_scope, backend: health.backend, latest_handshake: health.latest_handshake, slots: health.slots };
    result.ready = true;
    await atomicJSON(join(stateDir, 'pool.json'), result.pool);
    await atomicJSON(join(stateDir, 'fleet.json'), [result.pool]);
    if (!options.noConfigure) {
      try { result.configuration = await configurePool(result.pool, { rootDir, appURL: options.appURL, candidateCount: candidates.length, fetchImpl, readHealthImpl: dependencies.readHealth ?? readHealth }); result.configured = true; }
      catch (error) { result.configuration_error = error.message; }
    }
    if (result.configured && options.retireLegacy !== false) {
      result.legacy = await retireLegacyRelays(stateDir, command, fetchImpl);
    }
  }
  await atomicJSON(join(stateDir, 'setup-result.json'), result);
  return result;
}

export async function setupFleet(options = {}, dependencies = {}) {
  const opts = validateOptions(options);
  const rootDir = resolve(opts.rootDir ?? PROJECT_ROOT);
  const stateDir = resolve(opts.stateDir ?? join(rootDir, '.state/surfshark'));
  const command = dependencies.command ?? systemCommand;
  const fetchImpl = dependencies.fetchImpl ?? fetch;
  const waitHealthImpl = dependencies.waitHealth ?? waitHealth;
  const log = dependencies.log ?? (value => console.log(JSON.stringify(value)));
  const baseConfig = join(stateDir, 'base.conf'), catalogFile = join(stateDir, 'servers.json');
  const result = { configured: false, ready: false, requested_count: opts.count, fleet: [], startup_failures: [] };
  await protectDirectory(stateDir, command);
  await readFile(baseConfig);
  const candidates = candidatePool(JSON.parse(await readFile(catalogFile, 'utf8')));
  if (!candidates.length) throw new Error('No valid WireGuard candidates');
  if ((await command('docker', ['info', '--format', '{{.OSType}}'], { label: 'Check Docker engine' })).trim() !== 'linux') throw new Error('Docker Linux engine is required');
  if (opts.buildImage !== false) await buildImage(rootDir, stateDir, command, log);
  const usedHosts = new Set(), usedEndpoints = new Set(), usedIPs = new Set(), tried = new Set();
  for (let index = 1; index <= opts.count; index++) {
    const id = 'surfshark-' + index, name = ownedName('ocpool-surfshark-' + index);
    const port = opts.portBase + index, url = 'http://127.0.0.1:' + port;
    const tokenPath = join(stateDir, 'relay-' + index + '-token.txt'), envFile = join(stateDir, 'relay-' + index + '.env');
    const token = await relayToken(tokenPath);
    const existing = await containerInfo(name, command);
    let health, selected;
    if (existing && !opts.rebuild) {
      if (existing.status !== 'running') { result.startup_failures.push({ id, reason: 'Existing stopped container skipped; use --rebuild to replace it' }); continue; }
      try {
        health = await waitHealthImpl(url, token, null, usedIPs, { fetchImpl });
        selected = candidates.find(candidate => candidate.endpoint === health.vpn_endpoint);
        if (!selected || !verifiedHealth(health, selected, usedIPs) || usedHosts.has(selected.hostname) || usedEndpoints.has(selected.endpoint)) throw new Error('Existing fleet endpoint conflicts with another slot');
      } catch { result.startup_failures.push({ id, reason: 'Existing container did not verify distinct healthy egress; use --rebuild to replace it' }); continue; }
    } else {
      if (existing) await stopContainer(name, url, token, command, fetchImpl, existing.status === 'running');
      for (let attempt = 1; attempt <= opts.startupAttempts; attempt++) {
        selected = chooseCandidate(candidates, usedHosts, usedEndpoints, tried);
        if (!selected) { result.startup_failures.push({ id, reason: 'No unused server candidates remain' }); break; }
        tried.add(selected.id);
        await writeFile(envFile, 'OCPOOL_RELAY_TOKEN=' + token + '\nOCPOOL_VPN_BACKEND=wireguard\nOCPOOL_VPN_SERVER_ID=' + selected.id + '\n', { mode: 0o600 });
        log({ stage: 'start', id, attempt, vpn_server: selected.hostname, vpn_endpoint: selected.endpoint });
        try {
          await command('docker', dockerRunArgs({ name, port, envFile, baseConfig, catalogFile }), { label: 'Start dedicated WireGuard relay' });
          health = await waitHealthImpl(url, token, selected, usedIPs, { fetchImpl });
          if (!verifiedHealth(health, selected, usedIPs)) throw new Error('Candidate did not verify a distinct target egress IP');
          break;
        } catch (error) {
          health = undefined;
          result.startup_failures.push({ id, attempt, server_id: selected.id, vpn_server: selected.hostname, reason: error.message });
          const failed = await containerInfo(name, command);
          if (failed) await stopContainer(name, url, token, command, fetchImpl, failed.status === 'running');
        }
      }
    }
    if (!health || !selected) continue;
    usedHosts.add(selected.hostname); usedEndpoints.add(selected.endpoint); usedIPs.add(health.public_ip);
    const relativeToken = tokenPath.startsWith(rootDir + '\\') || tokenPath.startsWith(rootDir + '/') ? tokenPath.slice(rootDir.length + 1).replaceAll('\\', '/') : tokenPath;
    result.fleet.push({ id, container: name, url, token_file: relativeToken, server_id: selected.id, vpn_server: health.vpn_server || selected.hostname, vpn_endpoint: health.vpn_endpoint, public_ip: health.public_ip, ip_scope: health.ip_scope, backend: health.backend, latest_handshake: health.latest_handshake });
    await atomicJSON(join(stateDir, 'fleet.json'), result.fleet);
    log({ stage: 'ready', id, vpn_server: health.vpn_server, public_ip: health.public_ip, latest_handshake: health.latest_handshake });
  }
  result.ready = result.fleet.length === opts.count;
  if (result.ready && !opts.noConfigure) {
    try { result.configuration = await configureFleet(result.fleet, { rootDir, appURL: opts.appURL, expectedCount: opts.count, fetchImpl, readHealthImpl: dependencies.readHealth ?? readHealth }); result.configured = true; }
    catch (error) { result.configuration_error = error.message; }
  }
  await atomicJSON(join(stateDir, 'setup-result.json'), result);
  return result;
}

async function main() {
  // The complete-catalog single-container pool is the production default.
  // The bounded multi-container layout remains available only for migration
  // and comparison with the explicit --fleet flag.
  const options = { pool: true };
  for (let index = 2; index < process.argv.length; index++) {
    const flag = process.argv[index];
    if (flag === '--rebuild') options.rebuild = true;
    else if (flag === '--pool') options.pool = true;
    else if (flag === '--fleet') options.pool = false;
    else if (flag === '--no-configure') options.noConfigure = true;
    else if (flag === '--no-build') options.buildImage = false;
    else if (flag === '--configure-only') options.configureOnly = true;
    else if (flag === '--connections' || flag === '--pool-port' || flag === '--count' || flag === '--port-base' || flag === '--startup-attempts' || flag === '--app-url') {
      const value = process.argv[++index];
      if (!value) throw new Error('Missing fleet option value');
      if (flag === '--count') options.count = Number(value);
      else if (flag === '--port-base') options.portBase = Number(value);
      else if (flag === '--startup-attempts') options.startupAttempts = Number(value);
      else if (flag === '--pool-port') options.port = Number(value);
      else if (flag === '--connections') options.connections = Number(value);
      else options.appURL = value;
    } else throw new Error('Unknown fleet setup option');
  }
  if (options.configureOnly) {
    if (options.pool) {
      const pool = JSON.parse(await readFile(join(PROJECT_ROOT, '.state/surfshark/pool.json'), 'utf8'));
      const result = await configurePool(pool, { appURL: options.appURL, candidateCount: pool.candidate_count });
      const resultPath = join(PROJECT_ROOT, '.state/surfshark/setup-result.json');
      let previous = {};
      try { previous = JSON.parse(await readFile(resultPath, 'utf8')); } catch {}
      await atomicJSON(resultPath, { ...previous, ready: true, configured: true, pool, configuration: result, configured_at: new Date().toISOString() });
      console.log(JSON.stringify(result));
      return;
    }
    const fleet = JSON.parse(await readFile(join(PROJECT_ROOT, '.state/surfshark/fleet.json'), 'utf8'));
    const result = await configureFleet(fleet, { appURL: options.appURL, expectedCount: validateOptions(options).count });
    const resultPath = join(PROJECT_ROOT, '.state/surfshark/setup-result.json');
    let previous = {};
    try { previous = JSON.parse(await readFile(resultPath, 'utf8')); } catch {}
    await atomicJSON(resultPath, { ...previous, ready: true, configured: true, fleet, configuration: result, configured_at: new Date().toISOString() });
    console.log(JSON.stringify(result));
    return;
  }
  const result = options.pool ? await setupPool(options) : await setupFleet(options);
  console.log(JSON.stringify(result, null, 2));
  if (!result.ready || (!options.noConfigure && !result.configured)) process.exitCode = 1;
}
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(error => { console.error('Surfshark setup: ' + error.message); process.exitCode = 1; });
}
