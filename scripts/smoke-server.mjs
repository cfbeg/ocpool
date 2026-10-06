import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
const key = (await readFile('.state/server/admin-key.txt', 'utf8')).trim();
const base = 'http://127.0.0.1:10000';
const call = (path, body) => fetch(base + path, {
  method: body === undefined ? 'GET' : 'POST',
  headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
  ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: AbortSignal.timeout(120000)
});
assert.equal((await fetch(base + '/api/overview')).status, 401);
const models = await (await call('/v1/models')).json();
assert(models.data.some(model => model.id === 'opencode/big-pickle'));
const chatResponse = await call('/v1/chat/completions', { model: 'opencode/big-pickle', messages: [{ role: 'user', content: 'Reply with exactly SERVER_OK.' }] });
const chat = await chatResponse.json();
assert.equal(chatResponse.status, 200, JSON.stringify(chat));
assert.match(chat.choices[0].message.content, /SERVER_OK/);
console.log('chat: OK');
const streamResponse = await call('/v1/chat/completions', { model: 'opencode/big-pickle', messages: [{ role: 'user', content: 'Reply with exactly STREAM_OK.' }], stream: true });
const stream = await streamResponse.text();
assert.equal(streamResponse.status, 200, stream);
assert.match(stream, /\[DONE\]/);
let text = '';
for (const line of stream.split('\n')) if (line.startsWith('data: ') && line !== 'data: [DONE]') text += JSON.parse(line.slice(6)).choices?.[0]?.delta?.content || '';
assert.match(text, /STREAM_OK/);
console.log('stream: OK');
const firstResponse = await call('/v1/responses', { model: 'opencode/big-pickle', input: 'Remember the code word PINEAPPLE. Reply with exactly REMEMBERED.' });
const first = await firstResponse.json();
assert.equal(firstResponse.status, 200, JSON.stringify(first));
console.log('responses create: OK');
const secondResponse = await call('/v1/responses', { previous_response_id: first.id, input: 'What code word did I give you? Reply with just the code word.' });
const second = await secondResponse.json();
assert.equal(secondResponse.status, 200, JSON.stringify(second));
assert.match(second.output_text, /PINEAPPLE/i);
const report = { tested_at: new Date().toISOString(), model_count: models.data.length, chat: chat.choices[0].message.content, streaming: text, response_first: first.output_text, response_continuation: second.output_text };
await writeFile('.state/server/smoke-result.json', JSON.stringify(report, null, 2));
console.log(JSON.stringify(report, null, 2));
