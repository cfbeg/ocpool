import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { launchJobBrowser } from './job-browser.mjs';
const base='http://127.0.0.1:10000';
const browser=await launchJobBrowser('.state/research/dashboard');
try{
 const page=browser.pages()[0]||await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto((await readFile('.state/server/access-url.txt','utf8')).trim(),{waitUntil:'networkidle'});
 await page.locator('[data-view=jobs]').click();await page.locator('#job-filter').waitFor();
 await page.locator('#job-filter').selectOption('completed');await page.waitForResponse(r=>new URL(r.url()).pathname==='/api/jobs');
 await page.locator('[data-view=settings]').click();await page.locator('#settings-form').waitFor();
 const save=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/settings'&&r.request().method()==='PUT');
 await page.getByRole('button',{name:'変更を保存'}).click();assert.equal((await save).status(),200);
 await page.locator('[data-view=keys]').click();await page.locator('#new-key').click();await page.locator('#key-name').fill('UI verification');
 const create=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/keys'&&r.request().method()==='POST');
 await page.locator('#modal-submit').click();const created=await create;assert.equal(created.status(),201);const key=await created.json();
 assert.equal((await fetch(base+'/v1/models',{headers:{Authorization:`Bearer ${key.token}`}})).status,200);
 await page.locator('#close-modal').click();
 // Refresh the list after closing the one-time key display.
 await page.locator('[data-view=overview]').click();await page.locator('[data-view=keys]').click();
 const revoke=page.waitForResponse(r=>new URL(r.url()).pathname===`/api/keys/${key.id}`&&r.request().method()==='DELETE');
 await page.locator(`[data-revoke="${key.id}"]`).click();assert.equal((await revoke).status(),200);
 assert.equal((await fetch(base+'/v1/models',{headers:{Authorization:`Bearer ${key.token}`}})).status,401);
 assert.equal(errors.length,0,errors.join('\n'));
 console.log(JSON.stringify({job_filter:'ok',settings_save:'ok',api_key_create_revoke:'ok',page_errors:errors}));
}finally{await browser.close()}
