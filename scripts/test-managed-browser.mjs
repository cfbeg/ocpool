import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { launchJobBrowser } from './job-browser.mjs';
import { exposeManagedBrowser } from './managed-browser.mjs';
import { createServer } from 'node:http';

if(process.argv.includes('--fixture')){
 console.log(JSON.stringify({type:'starting'}));
 const server=createServer((_req,res)=>{res.setHeader('Content-Type','text/html');res.end('<!doctype html><html><body><button style="position:absolute;left:240px;top:170px;width:180px;height:70px" onclick="document.body.dataset.clicked=1">Click target</button></body></html>')});
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const browser=await launchJobBrowser('.state/research/input-fixture');
 console.log(JSON.stringify({type:'launched'}));
 const page=browser.pages()[0]||await browser.newPage();
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'domcontentloaded'});
 const stop=exposeManagedBrowser(page);
 const box=await page.locator('button').boundingBox();
 console.log(JSON.stringify({type:'target',x:box.x+box.width/2,y:box.y+box.height/2}));
 process.on('SIGTERM',async()=>{await stop();await browser.close();process.exit(0)});
 for(let i=0;i<100;i++){if(await page.evaluate(()=>document.body.dataset.clicked==='1')){console.log(JSON.stringify({type:'clicked'}));await delay(1000);break}await delay(100)}
 await stop();await browser.close();server.close();
}else{
 const child=spawn(process.execPath,[fileURLToPath(import.meta.url),'--fixture'],{stdio:['pipe','pipe','pipe'],windowsHide:true});
 let clicked=false,frame=false,stderr='',types=[];const lines=createInterface({input:child.stdout});
 child.stderr.on('data',chunk=>{stderr=(stderr+chunk.toString()).slice(-3000)});
 const complete=new Promise((resolve,reject)=>{
  lines.on('line',line=>{let v;try{v=JSON.parse(line)}catch{return}if(types.length<10)types.push(v.type);if(v.type==='target')child.stdin.write(JSON.stringify({type:'click',x:v.x,y:v.y})+'\n');if(v.type==='clicked')clicked=true;if(v.type==='frame')frame=true;if(clicked&&frame)resolve()});
  child.on('error',reject);child.on('exit',()=>{if(!clicked||!frame)reject(new Error('fixture exited before browser input succeeded'))});
 });
 try{await Promise.race([complete,delay(20000,undefined,{ref:false}).then(()=>{throw new Error('browser input timeout')})]);assert(clicked&&frame);console.log(JSON.stringify({managed_screenshot:'ok',mouse_click:'ok'}));}
 finally{if(!clicked||!frame)console.log(JSON.stringify({types,clicked,frame,stderr}));child.kill();lines.close()}
}
