import assert from 'node:assert/strict';
import { readFile,writeFile } from 'node:fs/promises';
const key=(await readFile('.state/server/admin-key.txt','utf8')).trim();
const reports=[];
async function chat(name,extra,prompt){
 const response=await fetch('http://127.0.0.1:10000/v1/chat/completions',{method:'POST',headers:{Authorization:`Bearer ${key}`,'Content-Type':'application/json'},body:JSON.stringify({model:'opencode/big-pickle',messages:[{role:'user',content:prompt}],...extra}),signal:AbortSignal.timeout(180000)});
 const raw=await response.text();assert.equal(response.status,200,`${name}: ${raw}`);
 if(extra.stream){assert.match(raw,/\[DONE\]/);const chunks=raw.split('\n').filter(l=>l.startsWith('data: ')&&l!=='data: [DONE]').map(l=>JSON.parse(l.slice(6)));assert(!chunks.some(c=>c.error),raw);const text=chunks.map(c=>c.choices?.[0]?.delta?.content||'').join('');reports.push({name,text});console.log(JSON.stringify(reports.at(-1)));return {text,chunks}}
 const value=JSON.parse(raw);reports.push({name,choices:value.choices,usage:value.usage});console.log(JSON.stringify(reports.at(-1)));return value;
}
const normal=await chat('temperature/max_tokens/stop:null',{temperature:0,top_p:1,max_tokens:128,stop:null,n:1},'Reply with exactly OPTIONS_OK.');assert.match(normal.choices[0].message.content,/OPTIONS_OK/);
const limited=await chat('max_completion_tokens=8',{temperature:0,max_completion_tokens:8},'Write 200 numbered lines, each containing a different English sentence.');assert.equal(limited.choices[0].finish_reason,'length');if(limited.usage)assert(limited.usage.completion_tokens<=8);
const stopped=await chat('stop/non-stream',{temperature:0,max_tokens:128,stop:['||END||']},'Print exactly PREFIX||END||SUFFIX and nothing else.');assert.match(stopped.choices[0].message.content,/PREFIX/);assert(!stopped.choices[0].message.content.includes('END')&&!stopped.choices[0].message.content.includes('SUFFIX'));
const stream=await chat('stop/stream',{temperature:0,max_tokens:128,stop:'||END||',stream:true},'Print exactly PREFIX||END||SUFFIX and nothing else.');assert.match(stream.text,/PREFIX/);assert(!stream.text.includes('END')&&!stream.text.includes('SUFFIX'));
const multiple=await chat('n=2',{temperature:0,max_tokens:512,n:2,stop:[]},'Reply with exactly CHOICE_OK.');assert.equal(multiple.choices.length,2);assert.deepEqual(multiple.choices.map(c=>c.index),[0,1]);assert(multiple.choices.every(c=>c.message.content.includes('CHOICE_OK')));
await writeFile('.state/server/generation-options-result.json',JSON.stringify(reports,null,2));
