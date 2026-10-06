import { randomInt, createHash } from 'node:crypto';
import { Resolver } from 'node:dns/promises';
import { isIP } from 'node:net';
import { execFile } from 'node:child_process';
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

export const CSV_URL = 'https://raw.githubusercontent.com/mthcht/awesome-lists/refs/heads/main/Lists/VPN/SurfSharkVPN/surfshark_vpn_servers_domains_and_ips_list.csv';
export const METADATA_URL = 'https://api.surfshark.com/v4/server/clusters';
export const WIREGUARD_PORT = 51820;

export function parseCSV(text) {
  const rows = [];
  let row = [], cell = '', quoted = false;
  for (let i = 0; i < text.length; i++) {
    const char = text[i];
    if (quoted) {
      if (char === '"' && text[i + 1] === '"') { cell += '"'; i++; }
      else if (char === '"') quoted = false;
      else cell += char;
    } else if (char === '"' && cell === '') quoted = true;
    else if (char === ',') { row.push(cell); cell = ''; }
    else if (char === '\n') { row.push(cell.replace(/\r$/, '')); rows.push(row); row = []; cell = ''; }
    else cell += char;
  }
  if (quoted) throw new Error('Unterminated CSV field');
  if (cell || row.length) { row.push(cell.replace(/\r$/, '')); rows.push(row); }
  return rows.filter(row => row.some(cell => cell.trim()));
}

export function validHostname(hostname) {
  return typeof hostname === 'string' && hostname.length <= 253 &&
    /^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+surfshark\.com$/.test(hostname);
}
export function publicIPv4(ip) {
  if (isIP(ip) !== 4) return false;
  const [a, b, c] = ip.split('.').map(Number);
  return a !== 0 && a !== 10 && a !== 127 && a < 224 &&
    !(a === 169 && b === 254) && !(a === 172 && b >= 16 && b <= 31) &&
    !(a === 192 && (b === 168 || b === 0)) && !(a === 100 && b >= 64 && b <= 127) &&
    !(a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) &&
    !(a === 203 && b === 0 && c === 113);
}
export function validPublicKey(key) {
  return typeof key === 'string' && /^[A-Za-z0-9+/]{43}=$/.test(key) &&
    Buffer.from(key, 'base64').length === 32 && Buffer.from(key, 'base64').toString('base64') === key;
}
export function parseServerCSV(text) {
  const rows = parseCSV(text.replace(/^\uFEFF/, ''));
  const header = rows.shift()?.map(cell => cell.trim().toLowerCase());
  const indexes = ['dest_nt_domain', 'dest_port', 'dest_ip'].map(name => header?.indexOf(name));
  if (indexes.some(index => index === undefined || index < 0)) throw new Error('CSV domain, port and IP headers are required');
  const servers = [], seen = new Set();
  let invalid = 0, duplicates = 0;
  for (const row of rows) {
    const hostname = row[indexes[0]]?.trim().toLowerCase();
    const csvPort = Number(row[indexes[1]]?.trim());
    const ip = row[indexes[2]]?.trim();
    if (!validHostname(hostname) || !Number.isInteger(csvPort) || csvPort < 1 || csvPort > 65535 || !publicIPv4(ip)) { invalid++; continue; }
    const key = hostname + '|' + ip;
    if (seen.has(key)) { duplicates++; continue; }
    seen.add(key);
    servers.push({ hostname, ip, csv_port: csvPort });
  }
  return { servers, row_count: rows.length, invalid_rows: invalid, duplicate_rows: duplicates };
}
export function shuffle(items, draw = randomInt) {
  const result = [...items];
  for (let i = result.length - 1; i > 0; i--) {
    const j = draw(i + 1);
    [result[i], result[j]] = [result[j], result[i]];
  }
  return result;
}
async function mapLimit(items, concurrency, action) {
  const results = new Array(items.length);
  let next = 0;
  await Promise.all(Array.from({ length: Math.min(concurrency, items.length) }, async () => {
    for (;;) {
      const index = next++;
      if (index >= items.length) return;
      results[index] = await action(items[index], index);
    }
  }));
  return results;
}
async function fetchText(url, fetchImpl, limit) {
  const parsed = new URL(url);
  if (parsed.protocol !== 'https:' || parsed.username || parsed.password) throw new Error('Catalog sources must use HTTPS');
  const response = await fetchImpl(url, { signal: AbortSignal.timeout(30_000), redirect: 'error', headers: { Accept: 'application/json,text/csv,text/plain' } });
  if (!response.ok) throw new Error('Catalog source failed: HTTP ' + response.status + ' at ' + parsed.hostname);
  let size = 0;
  const chunks = [];
  for await (const chunk of response.body) {
    size += chunk.length;
    if (size > limit) throw new Error('Catalog source exceeds size limit');
    chunks.push(Buffer.from(chunk));
  }
  return Buffer.concat(chunks).toString('utf8');
}
export async function resolveHost(hostname, { timeoutMs = 2500 } = {}) {
  const resolver = new Resolver({ timeout: timeoutMs, tries: 1 });
  const timer = setTimeout(() => resolver.cancel(), timeoutMs);
  try { return await resolver.resolve4(hostname); }
  finally { clearTimeout(timer); }
}

export function pingResult(output, exitCode) {
  if (exitCode !== 0) return { icmp_status: 'no_reply' };
  const time = output.match(/(?:time|時間|temps|zeit|tempo|tiempo)\s*([=<])\s*(\d+(?:[.,]\d+)?)\s*(?:ms|ミリ秒)/i);
  if (!time && !/ttl[=:\s]+\d+/i.test(output)) return { icmp_status: 'no_reply' };
  return { icmp_status: 'reply', ...(time ? { latency_ms: time[1] === '<' ? Number(time[2].replace(',', '.')) / 2 : Number(time[2].replace(',', '.')) } : {}) };
}
export async function pingCandidate(ip, { timeoutMs = 1500 } = {}) {
  if (!publicIPv4(ip)) throw new Error('Ping requires a public IPv4 address');
  const args = process.platform === 'win32'
    ? ['-4', '-n', '1', '-w', String(Math.max(100, timeoutMs - 250)), ip]
    : ['-4', '-c', '1', '-W', String(Math.max(1, Math.ceil(timeoutMs / 1000))), ip];
  return new Promise(resolvePing => {
    execFile('ping', args, { timeout: timeoutMs, windowsHide: true, maxBuffer: 32768 }, (error, stdout, stderr) => {
      if (error?.code === 'ENOENT') return resolvePing({ icmp_status: 'unavailable' });
      const exitCode = error ? (typeof error.code === 'number' ? error.code : -1) : 0;
      resolvePing(pingResult(String(stdout) + '\n' + String(stderr), exitCode));
    });
  });
}
export async function probeCandidates(entries, { count = 32, concurrency = 8, timeoutMs = 1500, ping = pingCandidate } = {}) {
  const probeCount = count === 'all' ? entries.length : count;
  if (!Number.isInteger(probeCount) || probeCount < 0 || probeCount > 2048 || !Number.isInteger(concurrency) || concurrency < 1 || concurrency > 32 || !Number.isInteger(timeoutMs) || timeoutMs < 100 || timeoutMs > 10_000) throw new Error('Invalid bounded ping options');
  const results = entries.map(entry => ({ ...entry, icmp_status: 'not_probed' }));
  const selected = results.slice(0, probeCount);
  await mapLimit(selected, concurrency, async entry => {
    try { Object.assign(entry, await ping(entry.endpoint.split(':')[0], { timeoutMs })); }
    catch { entry.icmp_status = 'probe_error'; }
  });
  return results;
}

export function inspectWireGuardConfig(text, metadata = []) {
  let section = '', endpoint, peerKey;
  let privateKeyPresent = false, presharedKeyPresent = false;
  for (const rawLine of text.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (line.startsWith('[')) { section = line.toLowerCase(); continue; }
    const index = line.indexOf('=');
    if (index < 0) continue;
    const key = line.slice(0, index).trim().toLowerCase();
    const value = line.slice(index + 1).trim();
    if (section === '[interface]' && key === 'privatekey') privateKeyPresent ||= !!value;
    if (section === '[peer]' && key === 'presharedkey') presharedKeyPresent ||= !!value;
    if (section === '[peer]' && key === 'endpoint') endpoint = value;
    if (section === '[peer]' && key === 'publickey') peerKey = value;
  }
  const hostname = endpoint?.split(':')[0].toLowerCase();
  const cluster = metadata.find(entry => entry.connectionName === hostname);
  return {
    endpoint, private_key_present: privateKeyPresent, preshared_key_present: presharedKeyPresent,
    public_key_valid: validPublicKey(peerKey),
    peer_public_key_fingerprint: peerKey ? createHash('sha256').update(peerKey).digest('hex').slice(0, 16) : null,
    metadata_endpoint_match: !!cluster,
    metadata_peer_key_match: !!cluster && peerKey === cluster.pubKey,
  };
}

export async function buildCatalog({ csvURL = CSV_URL, metadataURL = METADATA_URL, fetchImpl = fetch, dns = resolveHost, resolveDNS = true, dnsConcurrency = 12, probeCount = 32, probeConcurrency = 8, ping = pingCandidate, port = WIREGUARD_PORT, wgConfigPath } = {}) {
  if (!Number.isInteger(port) || port < 1 || port > 65535 || !Number.isInteger(dnsConcurrency) || dnsConcurrency < 1 || dnsConcurrency > 32) throw new Error('Invalid catalog options');
  const [csv, apiText] = await Promise.all([fetchText(csvURL, fetchImpl, 4 << 20), fetchText(metadataURL, fetchImpl, 8 << 20)]);
  const clusters = JSON.parse(apiText);
  if (!Array.isArray(clusters) || clusters.length > 10_000) throw new Error('Invalid Surfshark metadata');
  const parsed = parseServerCSV(csv);
  const clusterMap = new Map(), conflicts = new Set();
  for (const cluster of clusters) {
    const hostname = cluster.connectionName?.toLowerCase();
    if (!validHostname(hostname) || !validPublicKey(cluster.pubKey)) continue;
    if (clusterMap.has(hostname) && clusterMap.get(hostname).pubKey !== cluster.pubKey) conflicts.add(hostname);
    clusterMap.set(hostname, cluster);
  }
  for (const hostname of conflicts) clusterMap.delete(hostname);
  const hosts = new Map();
  for (const row of parsed.servers) {
    if (!hosts.has(row.hostname)) hosts.set(row.hostname, []);
    hosts.get(row.hostname).push(row.ip);
  }
  const stats = {
    csv_rows: parsed.row_count, csv_invalid_rows: parsed.invalid_rows, csv_duplicate_rows: parsed.duplicate_rows,
    csv_unique_hosts: hosts.size, metadata_clusters: clusters.length, valid_metadata_hosts: clusterMap.size,
    missing_or_invalid_metadata_hosts: 0, conflicting_metadata_hosts: conflicts.size,
    dns_failed_hosts: 0, stale_csv_ips: 0, current_dns_ips_added: 0,
  };
  const grouped = await mapLimit([...hosts], dnsConcurrency, async ([hostname, csvIPs]) => {
    const cluster = clusterMap.get(hostname);
    if (!cluster) { stats.missing_or_invalid_metadata_hosts++; return []; }
    let ips = csvIPs, dnsStatus = 'not_checked';
    if (resolveDNS) {
      try {
        ips = [...new Set(await dns(hostname))].filter(publicIPv4);
        if (!ips.length) throw new Error('No public IPv4 records');
      } catch { stats.dns_failed_hosts++; return []; }
      stats.stale_csv_ips += csvIPs.filter(ip => !ips.includes(ip)).length;
      stats.current_dns_ips_added += ips.filter(ip => !csvIPs.includes(ip)).length;
      dnsStatus = 'current_dns';
    }
    return [...new Set(ips)].map(ip => ({
      id: createHash('sha256').update(hostname + '|' + ip + '|' + port).digest('hex').slice(0, 24),
      hostname, endpoint: ip + ':' + port, public_key: cluster.pubKey,
      source: resolveDNS ? 'surfshark-api+csv-hostlist+current-dns' : 'surfshark-api+csv',
      country: cluster.country, country_code: cluster.countryCode, city: cluster.location,
      dns_status: dnsStatus,
    }));
  });
  const seen = new Map(), conflictEndpoints = new Set();
  for (const entry of grouped.flat()) {
    const old = seen.get(entry.endpoint);
    if (old && old.public_key !== entry.public_key) conflictEndpoints.add(entry.endpoint);
    else seen.set(entry.endpoint, entry);
  }
  for (const endpoint of conflictEndpoints) seen.delete(endpoint);
  const servers = await probeCandidates(shuffle([...seen.values()]), { count: probeCount, concurrency: probeConcurrency, ping });
  const metadata = {
    built_at: new Date().toISOString(), csv_url: csvURL, metadata_url: metadataURL,
    wireguard_port: port, csv_ports_used_for_wireguard: false,
    ...stats, conflicting_ip_endpoints: conflictEndpoints.size, server_count: servers.length,
    unique_peer_public_keys: new Set(servers.map(server => server.public_key)).size,
    dns_verified: resolveDNS,
    ping_probe_count: probeCount === 'all' ? servers.length : Math.min(probeCount, servers.length),
    catalog_complete: true,
    catalog_scope: resolveDNS ? 'current_dns' : 'csv',
    catalog_input_endpoint_count: parsed.servers.length,
    catalog_endpoint_count: servers.length,
    catalog_host_count: new Set(servers.map(server => server.hostname)).size,
    ping_reply_count: servers.filter(server => server.icmp_status === 'reply').length,
    ping_no_reply_count: servers.filter(server => server.icmp_status === 'no_reply').length,
    ping_note: 'ICMP is a candidate signal only. No reply does not prove offline. WireGuard handshake and tunnel trace remain required.',
  };
  if (wgConfigPath) metadata.provided_config = inspectWireGuardConfig(await readFile(wgConfigPath, 'utf8'), clusters);
  return { servers, metadata };
}
async function atomicJSON(path, value) {
  await mkdir(dirname(path), { recursive: true });
  const temp = path + '.' + process.pid + '.tmp';
  await writeFile(temp, JSON.stringify(value, null, 2) + '\n');
  await rename(temp, path);
}
async function main() {
  const values = new Map();
  for (let i = 2; i < process.argv.length; i += 2) {
    const key = process.argv[i], value = process.argv[i + 1];
    if (!['--csv-url', '--metadata-url', '--output', '--report', '--wg-config', '--probe-count'].includes(key) || value === undefined) throw new Error('Unknown or missing catalog argument');
    values.set(key, value);
  }
  const rawProbeCount = values.get('--probe-count');
  const probeCount = rawProbeCount === 'all' ? 'all' : Number(rawProbeCount ?? 32);
  const { servers, metadata } = await buildCatalog({
    csvURL: values.get('--csv-url') ?? CSV_URL, metadataURL: values.get('--metadata-url') ?? METADATA_URL,
    probeCount, wgConfigPath: values.get('--wg-config'),
  });
  if (!servers.length) throw new Error('No server candidates with correct metadata and current DNS');
  const output = values.get('--output') ?? '.state/surfshark/servers.json';
  const report = values.get('--report') ?? '.state/surfshark/catalog-metadata.json';
  await atomicJSON(output, servers);
  await atomicJSON(report, metadata);
  console.log(JSON.stringify({ output, report, ...metadata }, null, 2));
}
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(error => { console.error('Surfshark catalog: ' + error.message); process.exitCode = 1; });
}
