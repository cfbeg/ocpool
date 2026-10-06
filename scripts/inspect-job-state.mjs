import { DatabaseSync } from 'node:sqlite';
if (!process.argv[2]) throw new Error('Pass the job ID or prefix to inspect');
const db=new DatabaseSync('.state/server/pool.db',{readOnly:true});
const match=db.prepare('SELECT id,account_id,status,stage,error,kind,created_at,updated_at FROM jobs WHERE id LIKE ?').all(process.argv[2]+'%');
for(const job of match){console.log(JSON.stringify({job,related:db.prepare('SELECT id,status,stage,error,kind,created_at,updated_at FROM jobs WHERE account_id=? ORDER BY created_at,id').all(job.account_id),account:db.prepare('SELECT id,status,email FROM accounts WHERE id=?').get(job.account_id)},null,2))};db.close();
