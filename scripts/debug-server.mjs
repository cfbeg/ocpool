import { readFile } from 'node:fs/promises';
import { DatabaseSync } from 'node:sqlite';
const key = (await readFile('.state/server/admin-key.txt','utf8')).trim();
const response = await fetch('http://127.0.0.1:10000/api/overview',{headers:{Authorization:`Bearer ${key}`},signal:AbortSignal.timeout(10000)});
const overview=await response.json();
console.log(JSON.stringify({pending:overview.pending,runtimes:overview.runtimes},null,2));
const db=new DatabaseSync('.state/server/pool.db',{readOnly:true});
console.log(JSON.stringify({requests:db.prepare('SELECT model,status,milliseconds,error,created_at FROM requests ORDER BY created_at DESC LIMIT 10').all(),responses:db.prepare('SELECT id,account_id,session_id,model FROM responses').all()},null,2));
db.close();
