import { chromium } from 'playwright';
import { mkdir, writeFile } from 'node:fs/promises';

await mkdir('.state/research', { recursive: true });
const browser = await chromium.launch({ channel: 'msedge', headless: true });
try {
  const context = await browser.newContext();
  const page = await context.newPage();
  const requests = [];
  page.on('response', response => {
    const url = new URL(response.url());
    if (['xhr', 'fetch', 'document'].includes(response.request().resourceType()))
      requests.push({ method: response.request().method(), origin: url.origin, path: url.pathname, status: response.status() });
  });
  const signup = process.argv.includes('--signup');
  await page.goto(signup ? 'https://opencode.ai/console/signup' : 'https://opencode.ai/console/', { waitUntil: 'networkidle' });
  if (signup && await page.getByRole('button', { name: /email/i }).count()) {
    await page.getByRole('button', { name: /email/i }).click();
  }
  const summary = {
    url: page.url(), title: await page.title(), text: await page.locator('body').innerText(),
    inputs: await page.locator('input').evaluateAll(nodes => nodes.map(node => ({ name: node.name, type: node.type, placeholder: node.placeholder }))),
    buttons: await page.getByRole('button').allTextContents(),
    links: await page.locator('a').evaluateAll(nodes => nodes.map(node => ({ text: node.textContent, href: node.href }))), requests
  };
  await writeFile(`.state/research/console-${signup ? 'signup' : 'initial'}.json`, JSON.stringify(summary, null, 2));
  console.log(JSON.stringify(summary, null, 2));
  await page.screenshot({ path: `.state/research/console-${signup ? 'signup' : 'initial'}.png` });
} finally { await browser.close(); }
