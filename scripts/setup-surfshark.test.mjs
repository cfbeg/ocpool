import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve, sep } from 'node:path';
import { candidatePool, chooseCandidate, configureFleet, configurePool, dockerPoolArgs, dockerRunArgs, setupFleet, setupPool, validateOptions, validatePoolOptions, verifiedHealth, verifiedPoolHealth } from './setup-surfshark.mjs';

const publicKey = Buffer.alloc(32, 3).toString('base64');
const candidates = Array.from({ length: 10 }, (_, index) => ({ id: 'server_' + index, hostname: 'city-' + index + '.prod.surfshark.com', endpoint: '1.2.3.' + (index + 1) + ':51820', public_key: publicKey, icmp_status: 'reply' }));
const healthy = candidate => ({ healthy: true, connected: true, backend: 'wireguard', ip_scope: 'opencode.ai', public_ip: '203.0.113.' + candidate.endpoint.split('.')[3].split(':')[0], vpn_endpoint: candidate.endpoint, vpn_server: candidate.hostname, latest_handshake: Math.floor(Date.now() / 1000), active_flows: 0 });
const pooled = (count = 4) => ({ ...healthy(candidates[0]), pooled: true, pool_size: count, candidate_count: candidates.length, ready_count: count, cooling_count: 0, slots: candidates.slice(0, count).map((candidate, index) => ({ ...healthy(candidate), id: 'wg' + index })) });

async function fixture(t) {
  const directory = await mkdtemp(join(tmpdir(), 'ocpool-surfshark-setup-'));
  assert.ok(resolve(directory).startsWith(resolve(tmpdir()) + sep + 'ocpool-surfshark-setup-'));
  t.after(async () => {
    const target = resolve(directory);
    assert.ok(target.startsWith(resolve(tmpdir()) + sep + 'ocpool-surfshark-setup-'));
    await rm(target, { recursive: true, force: true });
  });
  const state = join(directory, '.state/surfshark');
  await mkdir(state, { recursive: true });
  await mkdir(join(directory, '.state/server'), { recursive: true });
  await writeFile(join(directory, '.state/server/admin-key.txt'), 'TEST_ADMIN_SECRET');
  await writeFile(join(state, 'base.conf'), '[Interface]\nPrivateKey = TEST_PRIVATE_SECRET\n');
  await writeFile(join(state, 'servers.json'), JSON.stringify(candidates));
  const inventory = new Map(), calls = [], controls = [];
  const command = async (file, args) => {
    calls.push({ file, args });
    if (file === 'powershell.exe') return 'S-1-5-21-12345';
    if (file === 'icacls.exe') return '';
    if (file !== 'docker') throw new Error('Unexpected command');
    if (args[0] === 'info') return 'linux';
    if (args[0] === 'ps') { const name = args[args.indexOf('--filter') + 1].slice('name=^/'.length, -1); return inventory.has(name) ? name : ''; }
    if (args[0] === 'inspect') return '{"ocpool.owner":"ocpool-surfshark-fleet"}|ocpool-surfshark:local|' + inventory.get(args.at(-1)).status;
    if (args[0] === 'run') {
      const name = args[args.indexOf('--name') + 1];
      const env = await readFile(args[args.indexOf('--env-file') + 1], 'utf8');
      const selectedID = env.match(/^OCPOOL_VPN_SERVER_ID=(.+)$/m)?.[1];
      inventory.set(name, { status: 'running', candidate: candidates.find(candidate => candidate.id === selectedID) ?? candidates[0] });
      return 'public-container-id';
    }
    if (args[0] === 'stop') { inventory.get(args.at(-1)).status = 'exited'; return ''; }
    if (args[0] === 'rm') { inventory.delete(args.at(-1)); return ''; }
    throw new Error('Unexpected Docker command');
  };
  const byURL = url => {
    const port = Number(new URL(url).port);
    return (port === 18100 ? inventory.get('ocpool-surfshark') : inventory.get('ocpool-surfshark-' + (port - 18100)))?.candidate;
  };
  const fetchImpl = async (url, options = {}) => {
    controls.push({ url, options });
    if (url.endsWith('/health')) return Response.json(Number(new URL(url).port) === 18100 ? pooled() : healthy(byURL(url)));
    if (url.endsWith('/drain')) return Response.json({ drained: true, active_flows: 0 });
    if (url.endsWith('/resume')) return Response.json({ resumed: true });
    throw new Error('Unexpected network request');
  };
  return { rootDir: directory, state, inventory, calls, controls, command, fetchImpl, byURL };
}

test('fleet options bound container count, ports and startup replacement attempts', () => {
  assert.equal(validateOptions({}).count, 3);
  assert.throws(() => validateOptions({ count: 17 }));
  assert.throws(() => validateOptions({ portBase: 65534, count: 3 }));
  assert.throws(() => validateOptions({ startupAttempts: 4 }));
});

test('pool connections default to four and are independently bounded from candidate count', () => {
  assert.equal(validatePoolOptions({}).connections, 4);
  assert.equal(validatePoolOptions({ connections: 32 }).connections, 32);
  for (const connections of [1, 33, 4.5, NaN]) assert.throws(() => validatePoolOptions({ connections }));
});

test('pool readiness requires two distinct verified exits and compatible configured pool size', () => {
  assert.ok(verifiedPoolHealth(pooled(), { expectedConnections: 4 }));
  const partial = pooled();
  partial.slots[2].healthy = false; partial.slots[3].healthy = false;
  assert.ok(verifiedPoolHealth(partial, { expectedConnections: 4 }));
  partial.slots[1].public_ip = partial.slots[0].public_ip;
  assert.equal(verifiedPoolHealth(partial), false);
  assert.equal(verifiedPoolHealth(pooled(), { expectedConnections: 8 }), false);
  assert.equal(verifiedPoolHealth(healthy(candidates[0])), false);
  assert.equal(verifiedPoolHealth({ ...pooled(), public_ip: '' }), false);
});

test('initial peers prefer ICMP replies and exclude assigned hostnames and endpoints', () => {
  const catalog = [{ ...candidates[0], icmp_status: 'not_probed' }, ...candidates.slice(1)];
  const pool = candidatePool(catalog);
  assert.equal(pool.at(-1).id, candidates[0].id);
  const first = pool[0];
  const selected = chooseCandidate(pool, new Set([first.hostname]), new Set([first.endpoint]), new Set());
  assert.notEqual(selected.hostname, first.hostname);
  assert.notEqual(selected.endpoint, first.endpoint);
  assert.ok(!verifiedHealth({ ...healthy(first), latest_handshake: 0 }, first));
  assert.ok(!verifiedHealth({ ...healthy(first), ip_scope: 'cloudflare.com' }, first));
  assert.ok(!verifiedHealth(healthy(first), first, new Set([healthy(first).public_ip])));
});

test('Docker plan isolates privileges, binds localhost, mounts read-only files and keeps secrets out of argv', () => {
  const args = dockerRunArgs({ name: 'ocpool-surfshark-1', port: 18101, envFile: 'private.env', baseConfig: 'base.conf', catalogFile: 'servers.json' });
  assert.ok(args.includes('NET_ADMIN'));
  assert.ok(args.includes('127.0.0.1:18101:18071'));
  assert.equal(args.filter(arg => arg.includes('readonly')).length, 2);
  assert.ok(!args.includes('--privileged'));
  assert.ok(!args.some(arg => arg.includes('SECRET')));
  assert.throws(() => dockerRunArgs({ name: 'other-user-container' }));
});

test('single pool Docker plan mounts the complete catalog once', () => {
  const args = dockerPoolArgs({ envFile: 'pool.env', baseConfig: 'base.conf', catalogFile: 'servers.json', stateDirectory: 'pool-state' });
  assert.ok(args.includes('--name') && args[args.indexOf('--name') + 1] === 'ocpool-surfshark');
  assert.ok(args.includes('127.0.0.1:18100:18071'));
  assert.equal(args.filter(arg => arg.includes('readonly')).length, 2);
  assert.ok(args.includes('type=bind,source=pool-state,target=/var/lib/ocpool'));
  assert.ok(!args.some(arg => arg.includes('SECRET')));
  assert.throws(() => dockerPoolArgs({}), /state directory/);
});

test('single pool configuration exposes one logical endpoint and disables prior routes', async t => {
  const f = await fixture(t);
  await writeFile(join(f.state, 'relay-pool-token.txt'), 'a'.repeat(64) + '\n');
  let saved;
  const pool = { id: 'surfshark-pool', url: 'http://127.0.0.1:18101', token_file: '.state/surfshark/relay-pool-token.txt', candidate_count: candidates.length };
  const fetchImpl = async (url, options = {}) => {
    if (options.method === 'PUT') { saved = JSON.parse(options.body); return Response.json({ config: saved }); }
    return Response.json({ config: { mode: 'spread', endpoints: [{ id: 'old-vpn', kind: 'vpn', url: 'http://127.0.0.1:18102', enabled: true }] } });
  };
  const result = await configurePool(pool, { rootDir: f.rootDir, fetchImpl, readHealthImpl: async () => pooled() });
  assert.equal(result.enabled_count, 1);
  assert.equal(saved.endpoints[0].id, 'surfshark-pool');
  assert.equal(saved.endpoints[0].enabled, true);
  assert.equal(saved.endpoints[0].rotate_on_limit, false);
  assert.equal(saved.endpoints[1].enabled, false);
  assert.equal(saved.mode, 'sticky');
  assert.equal(saved.allow_direct_fallback, false);
  assert.equal(saved.max_attempts, 1);
});

test('single pool starts one relay without pinning a server and records full catalog size', async t => {
  const f = await fixture(t);
  const result = await setupPool({ rootDir: f.rootDir, buildImage: false, noConfigure: true }, { command: f.command, fetchImpl: f.fetchImpl, log: () => {}, waitPoolHealth: async () => pooled() });
  assert.equal(result.ready, true);
  assert.equal(result.candidate_count, candidates.length);
  assert.equal(result.pool.id, 'surfshark-pool');
  assert.equal(result.pool.candidate_count, candidates.length);
  assert.equal(result.pool.connections, 4);
  assert.equal(result.pool.slots.length, 4);
  assert.equal(f.inventory.size, 1);
  const run = f.calls.find(call => call.args[0] === 'run' && call.args.includes('ocpool-surfshark'));
  assert.ok(run);
  const env = await readFile(join(f.state, 'relay-pool.env'), 'utf8');
  assert.ok(!env.includes('OCPOOL_VPN_SERVER_ID'));
  assert.ok(env.includes('OCPOOL_WG_POOL_SIZE=4\n'));
  assert.ok(env.includes('OCPOOL_VPN_STATE_FILE=/var/lib/ocpool/limits.json\n'));
  assert.ok(run.args.includes('type=bind,source=' + join(f.state, 'pool-state') + ',target=/var/lib/ocpool'));
  assert.ok(!JSON.stringify(result).includes('TEST_PRIVATE_SECRET'));
});

test('pool connections are configurable while the full catalog remains mounted', async t => {
  const f = await fixture(t);
  const result = await setupPool({ rootDir: f.rootDir, buildImage: false, noConfigure: true, connections: 6 }, { command: f.command, fetchImpl: f.fetchImpl, log: () => {}, waitPoolHealth: async () => pooled(6) });
  assert.equal(result.ready, true);
  assert.equal(result.connections, 6);
  assert.equal(result.candidate_count, candidates.length);
  assert.ok((await readFile(join(f.state, 'relay-pool.env'), 'utf8')).includes('OCPOOL_WG_POOL_SIZE=6\n'));
  assert.equal(JSON.parse(await readFile(join(f.state, 'servers.json'), 'utf8')).length, candidates.length);
});

test('pool setup never registers a single healthy exit as a multi-connection pool', async t => {
  const f = await fixture(t);
  const partial = pooled();
  partial.slots.slice(1).forEach(slot => { slot.healthy = false; });
  const result = await setupPool({ rootDir: f.rootDir, buildImage: false, noConfigure: false }, { command: f.command, fetchImpl: f.fetchImpl, log: () => {}, waitPoolHealth: async () => partial });
  assert.equal(result.ready, false);
  assert.equal(result.configured, false);
  assert.equal(result.startup_failures.length, 3);
  assert.equal(f.controls.filter(control => control.url.endsWith('/api/egress')).length, 0);
});

test('configured pool retires drained legacy containers and preserves ones without control tokens', async t => {
  const f = await fixture(t);
  f.inventory.set('ocpool-surfshark-1', { status: 'running', candidate: candidates[0] });
  f.inventory.set('ocpool-surfshark-2', { status: 'running', candidate: candidates[1] });
  await writeFile(join(f.state, 'relay-1-token.txt'), 'b'.repeat(64) + '\n');
  const fetchImpl = async (url, options = {}) => {
    if (url.endsWith('/api/egress')) return Response.json({ config: { mode: 'spread', endpoints: [] } });
    return f.fetchImpl(url, options);
  };
  const result = await setupPool({ rootDir: f.rootDir, buildImage: false }, { command: f.command, fetchImpl, log: () => {}, waitPoolHealth: async () => pooled(), readHealth: async () => pooled() });
  assert.equal(result.configured, true);
  assert.deepEqual(result.legacy.retired, ['ocpool-surfshark-1']);
  assert.equal(result.legacy.preserved[0].name, 'ocpool-surfshark-2');
  assert.ok(!f.inventory.has('ocpool-surfshark-1'));
  assert.ok(f.inventory.has('ocpool-surfshark-2'));
  assert.ok(!f.calls.some(call => call.args[0] === 'stop' && call.args.at(-1) === 'ocpool-surfshark-2'));
});

test('setup keeps successful relays, bounds replacements and never configures a partial fleet', async t => {
  const f = await fixture(t);
  let calls = 0;
  const result = await setupFleet({ rootDir: f.rootDir, buildImage: false, noConfigure: true }, {
    command: f.command, fetchImpl: f.fetchImpl, log: () => {},
    waitHealth: async (url, token, candidate) => { calls++; if (url.endsWith(':18103')) throw new Error('Candidate handshake unavailable'); return healthy(candidate); },
  });
  assert.equal(result.ready, false); assert.equal(result.configured, false);
  assert.equal(result.fleet.length, 2); assert.equal(calls, 5);
  assert.equal(result.startup_failures.length, 3);
  assert.equal(f.inventory.size, 2);
  assert.equal(f.calls.filter(call => call.args[0] === 'stop').length, 3);
  assert.equal(f.controls.filter(call => call.url.endsWith('/drain')).length, 3);
  const persisted = await readFile(join(f.state, 'fleet.json'), 'utf8');
  const report = await readFile(join(f.state, 'setup-result.json'), 'utf8');
  assert.ok(!persisted.includes('TEST_PRIVATE_SECRET')); assert.ok(!report.includes('TEST_ADMIN_SECRET'));
  const tokens = await Promise.all([1, 2, 3].map(index => readFile(join(f.state, 'relay-' + index + '-token.txt'), 'utf8')));
  assert.equal(new Set(tokens).size, 3);
  assert.ok(tokens.every(token => /^[a-f0-9]{64}\n$/.test(token)));
  assert.ok(tokens.every(token => !JSON.stringify(f.calls).includes(token.trim())));
});

test('existing healthy containers are reused and are not stopped without rebuild', async t => {
  const f = await fixture(t);
  const dependencies = { command: f.command, fetchImpl: f.fetchImpl, log: () => {}, waitHealth: async (url, token, candidate) => healthy(candidate ?? f.byURL(url)) };
  const options = { rootDir: f.rootDir, buildImage: false, noConfigure: true };
  const first = await setupFleet(options, dependencies);
  const second = await setupFleet(options, dependencies);
  assert.equal(first.ready, true); assert.equal(second.ready, true);
  assert.equal(new Set(second.fleet.map(item => item.public_ip)).size, 3);
  assert.equal(f.calls.filter(call => call.args[0] === 'run').length, 3);
  assert.equal(f.calls.filter(call => call.args[0] === 'stop').length, 0);
});

test('configuration enables only a complete distinct VPN fleet and preserves disabled WARP descriptor', async t => {
  const f = await fixture(t);
  const result = await setupFleet({ rootDir: f.rootDir, buildImage: false, noConfigure: true }, { command: f.command, fetchImpl: f.fetchImpl, log: () => {}, waitHealth: async (url, token, candidate) => healthy(candidate) });
  let saved;
  const fetchImpl = async (url, options) => {
    if (options.method === 'PUT') { saved = JSON.parse(options.body); return Response.json({ config: { mode: 'spread' } }); }
    return Response.json({ config: { mode: 'sticky', cooldown_seconds: 60, endpoints: [{ id: 'warp-1', kind: 'warp', url: 'http://127.0.0.1:18071', enabled: true, secret_configured: true }] } });
  };
  const configuration = await configureFleet(result.fleet, { rootDir: f.rootDir, fetchImpl, readHealthImpl: async url => healthy(f.byURL(url)) });
  assert.equal(configuration.mode, 'spread'); assert.equal(configuration.allow_direct_fallback, false);
  assert.equal(saved.endpoints.filter(endpoint => endpoint.enabled && endpoint.kind === 'vpn').length, 3);
  assert.equal(saved.endpoints.find(endpoint => endpoint.id === 'warp-1').enabled, false);
  assert.ok(!JSON.stringify(configuration).includes('TEST_ADMIN_SECRET'));
  await assert.rejects(configureFleet(result.fleet.slice(0, 2), { rootDir: f.rootDir }), /requested count/);
});
