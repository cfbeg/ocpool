import assert from 'node:assert/strict';
import { readFile,writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
const key=(await readFile('.state/server/admin-key.txt','utf8')).trim();
const base='http://127.0.0.1:10000';
const api=async(path,body)=>{const response=await fetch(base+path,{method:body?'POST':'GET',headers:{Authorization:`Bearer ${key}`,'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{}),signal:AbortSignal.timeout(20000)});const data=await response.json();assert(response.ok,JSON.stringify(data));return data};
const original=JSON.parse(await readFile('.state/server/web-creator-job.json','utf8'));
const job=await api(`/api/accounts/${original.account_id}/jobs`,{kind:'login'});
let last='';
for(let i=0;i<120;i++){
 const data=await api(`/api/jobs/${job.id}`);
 if(data.job.stage!==last){console.log(JSON.stringify({job:job.id,stage:data.job.stage}));last=data.job.stage}
 if(data.job.status==='completed'){
  assert(data.events.some(e=>e.stage==='oauth_saved_to_profile'));
  assert(!data.events.some(e=>e.stage==='oauth_saved_by_opencode'));
  const report={job_id:job.id,status:'completed',flow:'headless device OAuth',default_browser_launcher:false};
  await writeFile('.state/server/headless-oauth-result.json',JSON.stringify(report,null,2));console.log(JSON.stringify(report));process.exit(0);
 }
 if(['failed','needs_attention','cancelled'].includes(data.job.status))throw new Error(JSON.stringify(data.job));
 await delay(2000);
}
throw new Error('OAuth job timeout');
