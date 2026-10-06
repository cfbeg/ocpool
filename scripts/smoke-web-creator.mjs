import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
import { launchJobBrowser } from './job-browser.mjs';
const browser = await launchJobBrowser('.state/research/dashboard');
try {
 const page=browser.pages()[0]||await browser.newPage();
 const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto((await readFile('.state/server/access-url.txt','utf8')).trim(),{waitUntil:'networkidle'});
 let created;
 if(process.argv.includes('--resume')){
   const old=JSON.parse(await readFile('.state/server/web-creator-job.json','utf8'));
   await page.locator('[data-view=jobs]').click();
   await page.locator(`[data-job="${old.id}"]`).click();
   created=page.waitForResponse(r=>new URL(r.url()).pathname===`/api/jobs/${old.id}/resume`&&r.request().method()==='POST');
   await page.locator('#resume-job').click();
 }else{
   await page.getByRole('button',{name:'＋ アカウントを登録',exact:true}).click();
   await page.locator('#add-name').fill('Web verification');
   created=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/accounts'&&r.request().method()==='POST');
   await page.locator('#modal-submit').click();
 }
 const response=await created;assert.equal(response.status(),202);
 const data=await response.json();const job=Array.isArray(data)?data[0]:data;
 await writeFile('.state/server/web-creator-job.json',JSON.stringify(job,null,2));
 await page.locator('#job-status').waitFor();
 await page.waitForFunction(()=>{const img=document.querySelector('#browser-frame');return img && img.naturalWidth>0},{},{timeout:60000});
 await page.screenshot({path:'.state/research/dashboard-browser.png',fullPage:true});
 assert.equal(errors.length,0,errors.join('\n'));
 console.log(JSON.stringify({job_id:job.id,account_id:job.account_id,web_submission:'ok',browser_frame:'ok',page_errors:errors},null,2));
} finally{await browser.close()}
