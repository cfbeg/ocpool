import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { launchJobBrowser } from './job-browser.mjs';
const key=(await readFile('.state/server/admin-key.txt','utf8')).trim();
const base='http://127.0.0.1:10000';
const call=(path,body)=>fetch(base+path,{method:'POST',headers:{Authorization:`Bearer ${key}`,'Content-Type':'application/json'},body:JSON.stringify(body)});
const source=JSON.parse(await readFile('.state/server/web-creator-job.json','utf8'));
const created=await call(`/api/accounts/${source.account_id}/jobs`,{kind:'browser'});
assert.equal(created.status,202,await created.clone().text());
const job=await created.json();
const browser=await launchJobBrowser('.state/research/dashboard');
try{
 const page=browser.pages()[0]||await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto((await readFile('.state/server/access-url.txt','utf8')).trim(),{waitUntil:'networkidle'});
 await page.locator('[data-view=jobs]').click();
 await page.locator(`[data-job="${job.id}"]`).click();
 await page.waitForFunction(()=>document.querySelector('#browser-frame')?.naturalWidth>0,{},{timeout:60000});
 const frame=await page.request.get(`${base}/api/jobs/${job.id}/frame`);
 assert.equal(frame.status(),200);assert(Number(frame.headers()['x-viewport-width'])>0);
 await page.locator('[data-key=Tab]').click();
 await page.screenshot({path:'.state/research/dashboard-browser.png',fullPage:true});
 assert.equal(errors.length,0,errors.join('\n'));
 console.log(JSON.stringify({browser_frame:'ok',keyboard_input:'ok',job_id:job.id,page_errors:errors},null,2));
}finally{await browser.close();await call(`/api/jobs/${job.id}/cancel`,{});}
