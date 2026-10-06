import assert from 'node:assert/strict';
import { mkdir, readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { launchJobBrowser } from './job-browser.mjs';

// Live inspection is read-only. Editing and action coverage use local API fixtures.
await mkdir('.state/research', { recursive: true });
const fixtureOnly = process.argv.includes('--fixture-only');
let fixtureServer;
if (fixtureOnly) {
  fixtureServer = createServer(async (request, response) => {
    try {
      if (request.url.startsWith('/api/')) {
        response.setHeader('content-type', 'application/json');
        response.end(JSON.stringify(request.url.startsWith('/api/egress')
          ? { config: { mode: 'direct', allow_direct_fallback: false, max_attempts: 2, cooldown_seconds: 60, endpoints: [] }, routes: [], events: [] }
          : { accounts: {}, settings: {}, models: [] }));
        return;
      }
      const filename = request.url === '/app.js' ? 'app.js' : request.url === '/style.css' ? 'style.css' : 'index.html';
      response.setHeader('content-type', filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html');
      response.end(await readFile(`internal/app/web/${filename}`));
    } catch {
      response.writeHead(500); response.end();
    }
  });
  await new Promise(resolve => fixtureServer.listen(0, '127.0.0.1', resolve));
}
const browser = await launchJobBrowser('.state/research/egress-dashboard');
const accessURL = fixtureOnly ? `http://127.0.0.1:${fixtureServer.address().port}` : (await readFile('.state/server/access-url.txt', 'utf8')).trim();
const errors = [];
try {
  const page = browser.pages()[0] || await browser.newPage();
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(accessURL, { waitUntil: 'networkidle' });
  await page.locator('[data-view=egress]').click();
  await page.getByRole('heading', { name: '出口とIPの管理', exact: true }).waitFor();
  await page.locator('#egress-routes table').waitFor();
  assert.equal(await page.locator('#egress-form').count(), 1);
  assert.equal(await page.locator('#egress-unsaved').isVisible(), false);
  await page.screenshot({ path: fixtureOnly ? '.state/research/dashboard-egress-empty.png' : '.state/research/dashboard-egress-live.png', fullPage: true });

  const config = {
    mode: 'sticky', allow_direct_fallback: false, max_attempts: 2, cooldown_seconds: 60,
    endpoints: [
      { id: 'warp-fixture', name: 'WARP テスト', kind: 'warp', url: 'http://127.0.0.1:18071', enabled: true, secret_configured: true, rotate_on_limit: true },
      { id: 'proxy-fixture', name: 'Proxy テスト', kind: 'proxy', url: 'http://127.0.0.1:18080', enabled: true, secret_configured: true, rotate_on_limit: false },
      { id: 'vpn-fixture', name: 'VPN テスト', kind: 'vpn', url: 'http://127.0.0.1:18101', enabled: true, secret_configured: true, rotate_on_limit: true },
      { id: 'vpn-pool-fixture', name: 'VPN プール', kind: 'vpn', url: 'http://127.0.0.1:18102', enabled: true, secret_configured: true, rotate_on_limit: true },
    ],
  };
  let reads = 0, saved, unhealthyVPNRotation = false;
  const poolHealth = () => ({
    healthy: true, connected: true, pooled: true, backend: 'wireguard', public_ip: '192.0.2.100', ip_scope: 'opencode.ai',
    candidate_count: 120, pool_size: 3, ready_count: 2, cooling_count: 1,
    slots: [0, 1, 2].map(index => ({
      id: `wg${index}`, healthy: index !== 1, connected: true, public_ip: `192.0.2.${100 + index}`, ip_scope: 'opencode.ai', backend: 'wireguard',
      vpn_server: `vpn-${index}.example.com`, vpn_endpoint: `203.0.113.${100 + index}:51820`, latest_handshake: Math.floor(Date.now() / 1000) - 20,
      active_flows: index === 0 ? 2 : 0, rotating: false, cooldown_until: index === 1 ? Math.floor(Date.now() / 1000) + 90 : 0,
      last_error: index === 1 ? '利用制限のため休止中' : '', requests: 7, successes: 6, failures: 1, private_key: 'pool-private-fixture-value',
    })),
  });
  const actions = [];
  await page.route('**/api/egress**', async route => {
    const request = route.request();
    const url = new URL(request.url());
    if (request.method() === 'PUT') {
      saved = request.postDataJSON();
      const previous = config.endpoints;
      Object.assign(config, saved);
      config.endpoints = config.endpoints.map(({ secret, ...endpoint }) => ({ ...endpoint, secret_configured: !!secret || !!previous.find(item => item.id === endpoint.id)?.secret_configured }));
      return route.fulfill({ json: { ok: true } });
    }
    if (request.method() === 'POST') {
      actions.push({ path: url.pathname, body: request.postDataJSON() });
      if (url.pathname === '/api/egress/vpn-pool-fixture/probe') return route.fulfill({ json: poolHealth() });
      if (url.pathname === '/api/egress/vpn-pool-fixture/rotate') return route.fulfill({ status: 409, json: { error: { message: 'すべてのVPN接続が通信中です' } } });
      if (url.pathname === '/api/egress/vpn-fixture/rotate' && unhealthyVPNRotation) return route.fulfill({ json: { public_ip: '192.0.2.31', healthy: false, changed: false } });
      if (url.pathname.includes('/vpn-fixture/')) return route.fulfill({ json: { public_ip: '192.0.2.31', healthy: true, changed: !request.postDataJSON().same_server, vpn_endpoint: '203.0.113.11:51820', latest_handshake: Math.floor(Date.now() / 1000) } });
      return route.fulfill({ json: url.pathname.endsWith('/rotate') ? { public_ip: '192.0.2.20', healthy: true, changed: false } : { public_ip: '192.0.2.20', healthy: true } });
    }
    reads++;
    const routes = config.endpoints.filter(endpoint => endpoint.enabled).map(endpoint => ({
      ...endpoint, public_ip: '192.0.2.20', active: 1, requests: reads, successes: reads, failures: 1,
      cooldown_until: endpoint.kind === 'proxy' ? Math.floor(Date.now() / 1000) + 125 : 0,
      cooldowns: endpoint.kind === 'proxy' ? [{ model: 'opencode/test', until: Math.floor(Date.now() / 1000) + 125, scope: 'egress_model' }] : [],
      last_error: endpoint.kind === 'proxy' ? '利用制限のため休止中' : '', healthy: true, rotating: false, rotations: 1,
      ...(endpoint.kind === 'vpn' ? { backend: 'wireguard', vpn_server: 'jp-tok.prod.surfshark.com', vpn_endpoint: '203.0.113.10:51820', latest_handshake: Math.floor(Date.now() / 1000) - 15, ip_scope: 'opencode.ai' } : {}),
      ...(endpoint.id === 'vpn-pool-fixture' ? poolHealth() : {}),
    }));
    return route.fulfill({ json: {
      config, routes,
      events: [{ created_at: Math.floor(Date.now() / 1000), endpoint_id: 'warp-fixture', model: 'opencode/test', public_ip: '192.0.2.20', status: 429, kind: 'rate_limit', message: '出口を休止しました。' }],
    } });
  });
  await page.reload({ waitUntil: 'networkidle' });
  await page.locator('[data-view=egress]').click();
  await page.locator('#egress-routes').getByText('192.0.2.20', { exact: true }).first().waitFor();
  assert.equal(await page.locator('#egress-mode').inputValue(), 'sticky');
  assert.equal(await page.locator('#egress-routes').getByText(/分後に再開/).count(), 1);
  assert.equal(await page.locator('#egress-routes').getByText('一部モデル休止', { exact: true }).count(), 1);
  await page.locator('#egress-routes').getByText('サーバー: jp-tok.prod.surfshark.com', { exact: true }).waitFor();
  assert.equal(await page.locator('#egress-routes').getByText('接続先: 203.0.113.10:51820', { exact: true }).count(), 1);
  assert.equal(await page.locator('#egress-routes').getByText(/ハンドシェイク: \d+秒前/).count(), 1);
  assert.equal(await page.locator('[data-egress-renew="vpn-fixture"]').count(), 0, 'VPN must not expose WARP registration renewal');
  assert.equal(await page.locator('[data-egress-reconnect="warp-fixture"]').count(), 0, 'WARP must not expose VPN same-server reconnect');
  const pool = page.locator('[data-egress-pool="vpn-pool-fixture"]');
  assert.equal(await pool.locator('[data-vpn-slot]').count(), 3);
  assert.equal(await pool.locator('[data-vpn-slot="wg0"] .egress-slot-active').innerText(), '2');
  assert.equal(await pool.locator('[data-vpn-slot="wg1"]').getByText('休止中', { exact: true }).count(), 1);
  assert.equal(await pool.getByText('192.0.2.100', { exact: true }).count(), 1);
  assert.equal(await pool.getByText('192.0.2.101', { exact: true }).count(), 1);
  assert.equal(await pool.getByText('192.0.2.102', { exact: true }).count(), 1);
  assert.equal(await page.locator('#egress-routes').getByText('複数の出口IP', { exact: true }).count(), 1);
  assert(!(await page.locator('body').innerHTML()).includes('pool-private-fixture-value'));

  await page.locator('#egress-attempts').fill('3');
  const before = reads;
  await page.waitForTimeout(5500);
  assert(reads > before, 'Live status should update while a setting has focus');
  assert.equal(await page.locator('#egress-attempts').inputValue(), '3', 'Refresh must retain the draft');
  assert(await page.locator('#egress-unsaved').isVisible());

  await page.locator('[data-egress-edit="proxy-fixture"]').click();
  assert.equal(await page.locator('#endpoint-secret').inputValue(), '');
  assert.equal(await page.locator('#endpoint-secret-label').innerText(), 'プロキシ認証（ユーザー名:パスワード）');
  await page.locator('#endpoint-name').fill('Proxy 編集済み');
  await page.locator('#modal-submit').click();
  await page.locator('[data-egress-enabled="proxy-fixture"]').uncheck();

  await page.locator('[data-egress-edit="vpn-fixture"]').click();
  assert.equal(await page.locator('#endpoint-kind').inputValue(), 'vpn');
  assert.equal(await page.locator('#endpoint-secret').inputValue(), '');
  assert.equal(await page.locator('#endpoint-secret-label').innerText(), '認証トークン');
  assert.equal(await page.locator('#endpoint-rotate-label').innerText(), '利用制限時に次のVPNサーバーへ自動で切り替え');
  assert(await page.locator('#endpoint-rotate').isChecked());
  await page.locator('#modal-submit').click();

  await page.locator('#egress-add').click();
  await page.locator('#endpoint-name').fill('追加テスト');
  await page.locator('#endpoint-kind').selectOption('relay');
  await page.locator('#endpoint-url').fill('http://127.0.0.1:18090');
  await page.locator('#endpoint-secret').fill('fixture-private-value');
  await page.locator('#modal-submit').click();
  assert(!(await page.locator('body').innerText()).includes('fixture-private-value'));
  assert(!(await page.locator('body').innerHTML()).includes('fixture-private-value'));
  await page.locator('#egress-endpoints tr').filter({ hasText: '追加テスト' }).locator('[data-egress-delete]').click();
  await page.locator('#egress-save').click();
  await page.getByText('出口の設定を保存しました', { exact: true }).waitFor();
  assert.equal(saved.mode, 'sticky');
  assert.equal(saved.max_attempts, 3);
  assert.equal(saved.endpoints.length, 4);
  assert.equal(saved.endpoints.find(endpoint => endpoint.id === 'vpn-fixture').kind, 'vpn');
  assert.equal(saved.endpoints.find(endpoint => endpoint.id === 'vpn-fixture').rotate_on_limit, true);
  assert.equal(saved.endpoints.find(endpoint => endpoint.id === 'proxy-fixture').enabled, false);
  assert.equal(saved.endpoints.find(endpoint => endpoint.id === 'proxy-fixture').name, 'Proxy 編集済み');
  assert(saved.endpoints.every(endpoint => !('secret' in endpoint) && !('secret_configured' in endpoint)), 'Blank password fields must not overwrite stored credentials');

  await page.locator('[data-egress-probe="warp-fixture"]').click();
  await page.getByText('疎通確認が完了しました（192.0.2.20）', { exact: true }).waitFor();
  await page.locator('[data-egress-rotate="warp-fixture"]').click();
  await page.getByText('再接続しましたが、公開IPは変わりませんでした。休止時間を保持します。', { exact: true }).waitFor();
  await page.locator('.toast').evaluate(element => element.remove());
  await page.locator('[data-egress-renew="warp-fixture"]').click();
  await page.getByText('再接続しましたが、公開IPは変わりませんでした。休止時間を保持します。', { exact: true }).waitFor();
  await page.locator('[data-egress-rotate="vpn-fixture"]').click();
  await page.getByText('IPの切り替えが完了しました（192.0.2.31）', { exact: true }).waitFor();
  await page.locator('[data-egress-reconnect="vpn-fixture"]').click();
  await page.getByText('再接続しましたが、公開IPは変わりませんでした。休止時間を保持します。', { exact: true }).waitFor();
  unhealthyVPNRotation = true;
  await page.locator('[data-egress-rotate="vpn-fixture"]').click();
  await page.getByText('再接続後の出口を利用できません。接続状態を確認してください。', { exact: true }).waitFor();
  assert(await page.locator('.toast.error').isVisible(), 'An unhealthy HTTP 200 rotation must show an error');
  assert(!(await page.locator('.toast').innerText()).includes('公開IPは変わりませんでした'), 'Same-IP messaging is only valid for healthy rotation results');
  await page.locator('[data-egress-probe="vpn-pool-fixture"]').click();
  await page.getByText('疎通確認が完了しました（利用可能 2 / 3接続）', { exact: true }).waitFor();
  await page.locator('[data-egress-rotate="vpn-pool-fixture"]').click();
  await page.locator('.toast.error').getByText(/すべてのVPN接続が通信中です/).waitFor();
  assert.equal(await pool.locator('[data-vpn-slot="wg0"] .egress-slot-active').innerText(), '2', 'Busy rejection must retain the active slot display');
  assert.deepEqual(actions, [
    { path: '/api/egress/warp-fixture/probe', body: {} },
    { path: '/api/egress/warp-fixture/rotate', body: {} },
    { path: '/api/egress/warp-fixture/rotate', body: { renew_registration: true, max_attempts: 1 } },
    { path: '/api/egress/vpn-fixture/rotate', body: {} },
    { path: '/api/egress/vpn-fixture/rotate', body: { same_server: true, max_attempts: 1 } },
    { path: '/api/egress/vpn-fixture/rotate', body: {} },
    { path: '/api/egress/vpn-pool-fixture/probe', body: {} },
    { path: '/api/egress/vpn-pool-fixture/rotate', body: {} },
  ]);
  await page.locator('.toast').evaluate(element => element.remove());
  await page.evaluate(() => scrollTo(0, 0));
  await page.screenshot({ path: '.state/research/dashboard-egress-fixture.png', fullPage: true });
  await page.setViewportSize({ width: 390, height: 550 });
  await page.evaluate(() => scrollTo(0, 0));
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'Mobile layout must not overflow the page');
  await page.screenshot({ path: '.state/research/dashboard-egress-mobile.png', fullPage: true });
  assert.equal(errors.length, 0, errors.join('\n'));
  console.log(JSON.stringify({ live_readonly: fixtureOnly ? 'skipped' : 'ok', draft_retention: 'ok', endpoint_editing: 'ok', credentials_hidden: 'ok', probe_and_rotation_ui: 'ok', vpn_handshake_and_rotation: 'ok', unhealthy_rotation_error: 'ok', pooled_slot_status: 'ok', busy_rotation_preserves_stream: 'ok', mobile: 'ok', page_errors: errors }));
} finally {
  await browser.close();
  if (fixtureServer) await new Promise(resolve => fixtureServer.close(resolve));
}
