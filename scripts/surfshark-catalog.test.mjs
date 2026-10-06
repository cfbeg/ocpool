import test from 'node:test';
import assert from 'node:assert/strict';
import { buildCatalog, inspectWireGuardConfig, parseServerCSV, pingResult, probeCandidates, shuffle } from './surfshark-catalog.mjs';

const keyA = Buffer.alloc(32, 1).toString('base64');
const keyB = Buffer.alloc(32, 2).toString('base64');
const metadataURL = 'https://api.surfshark.com/v4/server/clusters';
const csvURL = 'https://fixture.example/list.csv';
const fixtureFetch = (csv, metadata) => async url => new Response(url === metadataURL ? JSON.stringify(metadata) : csv);

test('CSV rejects non-provider domains, private IPs and invalid ports while keeping valid unique rows', () => {
  const text = '\uFEFFdest_nt_domain,dest_port,dest_ip\r\n"jp-tok.prod.surfshark.com",1194,1.2.3.4\r\njp-tok.prod.surfshark.com,1194,1.2.3.4\r\nevil.example,1194,2.3.4.5\r\njp-tok.prod.surfshark.com,1194,127.0.0.1\r\njp-tok.prod.surfshark.com,99999,2.3.4.5\r\n';
  const result = parseServerCSV(text);
  assert.equal(result.servers.length, 1);
  assert.equal(result.invalid_rows, 3);
  assert.equal(result.duplicate_rows, 1);
});

test('catalog joins each host to its own public key and current DNS, using WG port instead of CSV OpenVPN port', async () => {
  const csv = 'dest_nt_domain,dest_port,dest_ip\njp-tok.prod.surfshark.com,1194,1.2.3.4\nus-nyc.prod.surfshark.com,1194,2.3.4.5\nde-fra.prod.surfshark.com,1194,3.4.5.6\n';
  const clusters = [
    { connectionName: 'jp-tok.prod.surfshark.com', pubKey: keyA, country: 'Japan' },
    { connectionName: 'us-nyc.prod.surfshark.com', pubKey: keyB, country: 'United States' },
    { connectionName: 'de-fra.prod.surfshark.com', pubKey: '' },
  ];
  const { servers, metadata } = await buildCatalog({ csvURL, fetchImpl: fixtureFetch(csv, clusters), probeCount: 0, dns: async hostname => hostname.startsWith('jp') ? ['8.8.8.8', '127.0.0.1'] : ['2.3.4.5'] });
  assert.equal(servers.length, 2);
  assert.equal(servers.find(server => server.country === 'Japan').endpoint, '8.8.8.8:51820');
  assert.equal(servers.find(server => server.country === 'Japan').public_key, keyA);
  assert.equal(servers.find(server => server.country === 'United States').public_key, keyB);
  assert.equal(metadata.missing_or_invalid_metadata_hosts, 1);
  assert.equal(metadata.stale_csv_ips, 1);
  assert.equal(metadata.catalog_complete, true);
  assert.equal(metadata.catalog_endpoint_count, 2);
  assert.equal(metadata.catalog_host_count, 2);
});

test('conflicting metadata keys for one host and conflicting keys for one IP do not produce unsafe peers', async () => {
  const csv = 'dest_nt_domain,dest_port,dest_ip\njp-tok.prod.surfshark.com,1194,1.2.3.4\nus-nyc.prod.surfshark.com,1194,1.2.3.4\n';
  const clusters = [{ connectionName: 'jp-tok.prod.surfshark.com', pubKey: keyA }, { connectionName: 'us-nyc.prod.surfshark.com', pubKey: keyB }];
  const conflictIP = await buildCatalog({ csvURL, fetchImpl: fixtureFetch(csv, clusters), resolveDNS: false, probeCount: 0 });
  assert.equal(conflictIP.servers.length, 0);
  assert.equal(conflictIP.metadata.conflicting_ip_endpoints, 1);
  const conflictHost = await buildCatalog({ csvURL, fetchImpl: fixtureFetch(csv, [...clusters, { connectionName: 'jp-tok.prod.surfshark.com', pubKey: keyB }]), resolveDNS: false, probeCount: 0 });
  assert.equal(conflictHost.metadata.conflicting_metadata_hosts, 1);
  assert.equal(conflictHost.servers.length, 1);
});

test('ICMP probing obeys count and concurrency bounds without treating silence as offline', async () => {
  const entries = Array.from({ length: 8 }, (_, index) => ({ id: String(index), endpoint: '1.2.3.' + (index + 1) + ':51820' }));
  let active = 0, peak = 0, calls = 0;
  const results = await probeCandidates(entries, { count: 5, concurrency: 2, ping: async () => {
    calls++; active++; peak = Math.max(peak, active);
    await new Promise(resolve => setTimeout(resolve, 5)); active--;
    return { icmp_status: 'no_reply' };
  } });
  assert.equal(calls, 5); assert.equal(peak, 2);
  assert.equal(results.filter(result => result.icmp_status === 'not_probed').length, 3);
  assert.ok(results.every(result => !('healthy' in result)));
  assert.equal(pingResult('Reply from 1.2.3.4: bytes=32 time=15ms TTL=54', 0).latency_ms, 15);
  assert.equal(pingResult('1.2.3.4 からの応答: バイト数 =32 時間 <1ms TTL=54', 0).icmp_status, 'reply');
  assert.equal(pingResult('Destination host unreachable', 0).icmp_status, 'no_reply');
});

test('all probe mode measures every catalog endpoint without truncating inventory', async () => {
  const entries = Array.from({ length: 6 }, (_, index) => ({ id: String(index), endpoint: '1.2.3.' + (index + 1) + ':51820' }));
  let calls = 0;
  const results = await probeCandidates(entries, { count: 'all', concurrency: 3, ping: async () => { calls++; return { icmp_status: 'reply' }; } });
  assert.equal(calls, entries.length);
  assert.equal(results.length, entries.length);
  assert.ok(results.every(result => result.icmp_status === 'reply'));
});

test('config inspection returns public metadata and a key comparison without exposing private or preshared keys', () => {
  const text = '[Interface]\r\nPrivateKey = SECRET_PRIVATE\r\n[Peer]\r\nEndpoint = jp-tok.prod.surfshark.com:51820\r\nPublicKey = ' + keyA + '\r\nPresharedKey = SECRET_PRESHARED\r\n';
  const result = inspectWireGuardConfig(text, [{ connectionName: 'jp-tok.prod.surfshark.com', pubKey: keyA }]);
  assert.equal(result.metadata_peer_key_match, true);
  assert.equal(result.private_key_present, true);
  assert.equal(result.preshared_key_present, true);
  assert.ok(!JSON.stringify(result).includes('SECRET_'));
  const shuffled = shuffle([1, 2, 3, 4]);
  assert.deepEqual(shuffled.toSorted(), [1, 2, 3, 4]);
});
