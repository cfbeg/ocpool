import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
const key=(await readFile('.state/server/admin-key.txt','utf8')).trim();
const response=await fetch('http://127.0.0.1:10000/v1/chat/completions',{method:'POST',headers:{Authorization:`Bearer ${key}`,'Content-Type':'application/json'},body:JSON.stringify({model:'opencode/big-pickle',messages:[{role:'user',content:'Reply with exactly FINAL_OK.'}],stream:true,stream_options:{include_usage:true}}),signal:AbortSignal.timeout(180000)});
const raw=await response.text();assert.equal(response.status,200,raw);assert.match(raw,/\[DONE\]/);
const chunks=raw.split('\n').filter(line=>line.startsWith('data: ')&&line!=='data: [DONE]').map(line=>JSON.parse(line.slice(6)));
const text=chunks.map(v=>v.choices?.[0]?.delta?.content||'').join('');assert.match(text,/FINAL_OK/);
assert(chunks.some(v=>v.usage&&v.choices.length===0));console.log(JSON.stringify({streaming:text,usage_event:'ok'}));
