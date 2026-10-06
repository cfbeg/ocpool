import { launchJobBrowser } from './job-browser.mjs';
import { mkdir, readFile, writeFile, rename } from 'node:fs/promises';
import { randomBytes } from 'node:crypto';
import { resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { authorizeCLI, probeAccount } from './oauth-cli.mjs';
import { exposeManagedBrowser } from './managed-browser.mjs';

const flag = name => {
  const index = process.argv.indexOf(name);
  return index < 0 ? undefined : process.argv[index + 1];
};
const id = flag('--id') || `test-${new Date().toISOString().slice(0, 10)}-${randomBytes(4).toString('hex')}`;
const humanWait = process.argv.includes('--human-wait');
const managed = process.argv.includes('--managed');
const browserOnly = process.argv.includes('--browser-only');
const currentRun = flag('--run-id');
if (humanWait && !process.argv.includes('--headed') && !managed) throw new Error('--human-wait requires --headed or --managed');
if (!/^[a-zA-Z0-9_-]{1,100}$/.test(id)) throw new Error('Invalid job id');
const directory = resolve('.state/onboarding', id);
await mkdir(directory, { recursive: true, mode: 0o700 });
const file = resolve(directory, 'job.json');
const bridge = JSON.parse(await readFile('.state/email-bridge.json', 'utf8'));
let job;
try { job = JSON.parse(await readFile(file, 'utf8')); }
catch (error) { if (error.code !== 'ENOENT') throw error; }
job ??= {
  id, email: flag('--email') || `ocp-${randomBytes(8).toString('hex')}@${bridge.mail_domain}`,
  password: `OcP!${randomBytes(24).toString('base64url')}`, stage: 'new', created_at: new Date().toISOString()
};
if (!job.email.toLowerCase().endsWith(`@${bridge.mail_domain}`)) throw new Error('Unexpected email domain');
const save = async stage => {
  if (stage) job.stage = stage;
  if (stage === 'ready' && currentRun) { job.completed_run = currentRun; job.completed_at = new Date().toISOString(); }
  if (['browser_verified', 'oauth_verified', 'ready'].includes(stage)) {
    delete job.last_error;
    delete job.pending_action;
  }
  job.updated_at = new Date().toISOString();
  const temporary = `${file}.${process.pid}.tmp`;
  await writeFile(temporary, JSON.stringify(job, null, 2), { mode: 0o600 });
  await rename(temporary, file);
};
const status = (stage, extra = {}) => console.log(JSON.stringify({ job: id, email: job.email, stage, ...extra }));
const mailbox = async (path, options = {}) => {
  const response = await fetch(`${bridge.base_url}${path}`, {
    ...options, signal: AbortSignal.timeout(20000),
    headers: { Authorization: `Bearer ${bridge.api_token}`, 'Content-Type': 'application/json' }
  });
  if (!response.ok) throw new Error(`mailbox_http_${response.status}`);
  return response.status === 204 ? undefined : response.json();
};
await save();
if (process.argv.includes('--status')) { status(job.stage, { directory }); process.exit(0); }
if (process.argv.includes('--login')) { await save('browser_login_required'); }
const finishProbe = async () => {
  status('sdk_probe_starting');
  const probe = await probeAccount(directory, { prompt: true });
  job.probe = probe;
  await save(probe.text ? 'ready' : 'oauth_verified');
  status(job.stage, { probe });
  if (!probe.text) process.exitCode = 2;
};
if (job.stage === 'ready' && !process.argv.includes('--recheck') && !browserOnly) {
  if (currentRun) await save('ready');
  status(job.stage, { probe: job.probe });
  process.exit(0);
}
if (!browserOnly && (job.stage === 'oauth_verified' || (job.stage === 'ready' && process.argv.includes('--recheck')))) {
  try { await finishProbe(); }
  catch (error) { job.last_error = error.message; await save(); status('error', { reason: error.message }); process.exitCode = 1; }
  process.exit(process.exitCode || 0);
}
if (job.stage === 'signup_action_required' && job.pending_action === 'captcha' && !humanWait) {
  status(job.stage, { pending_action: 'captcha', resumable: true });
  process.exit(2);
}

// Each job owns a persistent browser context; no user browser profile is read.
const browser = await launchJobBrowser(directory, { headed: process.argv.includes('--headed') });
const page = browser.pages()[0] || await browser.newPage();
const stopRemote = managed ? exposeManagedBrowser(page) : async () => {};
page.setDefaultTimeout(20000);
const snapshot = async label => {
  if (page.isClosed()) return;
  await page.screenshot({ path: resolve(directory, `${label}.png`), fullPage: true });
  await writeFile(resolve(directory, `${label}.json`), JSON.stringify({
    path: new URL(page.url()).pathname,
    text: await page.locator('body').innerText(),
    dimensions: await page.evaluate(() => ({ width: innerWidth, height: innerHeight, outerWidth, outerHeight, ratio: devicePixelRatio })),
    inputs: await page.locator('input').evaluateAll(nodes => nodes.map(node => ({ type: node.type, name: node.name, placeholder: node.placeholder })))
  }, null, 2), { mode: 0o600 });
};
try {
  if (browserOnly) {
    await page.goto('https://opencode.ai/console/', { waitUntil: 'networkidle' });
    status('browser_open');
    const deadline = Date.now() + 15 * 60 * 1000;
    while (!page.isClosed() && Date.now() < deadline) await delay(1000);
  }
  if (job.stage === 'browser_login_required') {
    await page.goto('https://opencode.ai/console/', { waitUntil: 'networkidle' });
    let session = await page.request.get('https://opencode.ai/console/auth/session');
    if (!session.ok()) {
      if (await page.getByRole('button', { name: 'Continue with email', exact: true }).count())
        await page.getByRole('button', { name: 'Continue with email', exact: true }).click();
      if (await page.locator('input[type=email]').count()) await page.locator('input[type=email]').fill(job.email);
      if (job.password && await page.locator('input[type=password]').count()) {
        await page.locator('input[type=password]').fill(job.password);
        const login = page.getByRole('button', { name: /^(Log in|Sign in|Continue)$/i });
        if (await login.count()) await login.first().click();
      }
      status('waiting_for_browser_confirmation');
      const deadline = Date.now() + 15 * 60 * 1000;
      while (Date.now() < deadline) {
        session = await page.request.get('https://opencode.ai/console/auth/session');
        if (session.ok()) break;
        await delay(3000);
      }
    }
    if (!session.ok()) throw new Error('browser_login_timeout');
    const identity = await session.json();
    if ((identity.user || identity).email?.toLowerCase() !== job.email.toLowerCase()) throw new Error('browser_identity_mismatch');
    await save('browser_verified');
  }
  if (['new', 'signup_action_required'].includes(job.stage) || (job.stage === 'signup_submitting' && process.argv.includes('--retry-signup'))) {
    if (!job.inbox || job.inbox.expires_at * 1000 <= Date.now()) {
      job.inbox = await mailbox('/v1/inboxes', { method: 'POST', body: JSON.stringify({ email: job.email, ttl_seconds: 3600 }) });
      await save();
    }
    await page.goto('https://opencode.ai/console/signup', { waitUntil: 'networkidle' });
    await page.locator('input[type=email]').fill(job.email);
    await page.locator('input[type=password]').fill(job.password);
    const registration = page.waitForResponse(response => new URL(response.url()).pathname === '/console/auth/register', {
      timeout: humanWait ? 15 * 60 * 1000 : 30000
    }).catch(() => null);
    await save('signup_submitting');
    await page.getByRole('button', { name: 'Create Account', exact: true }).click();
    if (humanWait) {
      status('waiting_for_browser_confirmation', { browser: 'cloakbrowser', timeout_seconds: 900 });
      await delay(1500);
      await snapshot('confirmation-pending').catch(() => {});
    }
    const response = await registration;
    if (response) {
      job.registration_status = response.status();
      if (!response.ok()) {
        const details = await response.json().catch(() => ({}));
        job.registration_error = { tag: details._tag || details.error || '', message: details.message || '' };
        delete job.pending_action;
        await snapshot('signup-response');
        await save('signup_action_required');
        status(job.stage, { http_status: response.status(), error: job.registration_error });
        process.exitCode = 2;
      } else {
        await page.waitForURL(url => url.pathname !== '/console/signup', { timeout: 15000 }).catch(() => {});
        delete job.registration_error;
        delete job.pending_action;
        await save('verification_pending');
        await snapshot('signup-submitted');
        status(job.stage);
      }
    } else {
      await snapshot('signup-interaction');
      const captchaFrame = !page.isClosed() && page.frames().some(frame => /hcaptcha\.com/.test(frame.url()) && /challenge/.test(frame.url()));
      job.pending_action = captchaFrame ? 'captcha' : 'signup_review';
      await save('signup_action_required');
      status(job.stage, { pending_action: job.pending_action, reason: 'Signup did not complete; inspect the saved browser state and screenshot.' });
      process.exitCode = 2;
    }
  }

  if (job.stage === 'verification_pending') {
    if (job.inbox.expires_at * 1000 <= Date.now()) {
      job.inbox = await mailbox('/v1/inboxes', { method: 'POST', body: JSON.stringify({ email: job.email, ttl_seconds: 3600 }) });
      await save();
      await page.goto('https://opencode.ai/console/check-email', { waitUntil: 'networkidle' });
      const resend = page.getByRole('button', { name: 'Resend email', exact: true });
      if (await resend.count()) await resend.click();
    }
    status('waiting_for_verification_email');
    const deadline = Date.now() + Number(flag('--mail-timeout') || 180) * 1000;
    while (Date.now() < deadline) {
      const result = await mailbox(`/v1/inboxes/${job.inbox.id}/messages`);
      const link = result.messages.flatMap(message => message.urls).find(value => {
        const url = new URL(value);
        return url.origin === 'https://opencode.ai' && url.pathname === '/console/verify-email' && url.searchParams.has('token');
      });
      if (link) {
        // The email link is opened inside the same browser context as registration.
        await page.goto(link, { waitUntil: 'networkidle' });
        await page.waitForURL(url => url.pathname !== '/console/verify-email', { timeout: 20000 }).catch(() => {});
        await snapshot('verification-result');
        if (new URL(page.url()).pathname === '/console/verify-email') {
          await save('verification_action_required');
          status(job.stage);
          process.exitCode = 2;
        } else {
          const auth = await page.request.get('https://opencode.ai/console/auth/session');
          job.browser_session_status = auth.status();
          if (auth.ok()) {
            const session = await auth.json();
            // Persist only the identity, not a duplicate of session credentials.
            const identity = session.user || session;
            job.browser_email = identity.email;
            if (job.browser_email && job.browser_email.toLowerCase() !== job.email.toLowerCase()) throw new Error('browser_identity_mismatch');
          }
          await save(auth.ok() ? 'browser_verified' : 'browser_login_required');
          status(job.stage, { session_status: auth.status() });
          await mailbox(`/v1/inboxes/${job.inbox.id}`, { method: 'DELETE' });
        }
        break;
      }
      await delay(2000);
    }
    if (job.stage === 'verification_pending') {
      status(job.stage, { reason: 'mail_timeout', resumable: true });
      process.exitCode = 2;
    }
  }
  if (job.stage === 'browser_verified' && !process.argv.includes('--signup-only')) {
    status('oauth_starting');
    await authorizeCLI({ directory, page, email: job.email, report: stage => status(stage) });
    await save('oauth_verified');
  }
  if (job.stage === 'oauth_verified') {
    await finishProbe();
  }
} catch (error) {
  job.last_error = error.message;
  await save();
  await snapshot('failure').catch(() => {});
  status('error', { reason: error.message });
  process.exitCode = 1;
} finally { await stopRemote(); await browser.close().catch(() => {}); }
