import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';

const key = (await readFile('.state/server/admin-key.txt', 'utf8')).trim();
const model = 'opencode/mimo-v2.6-flash-free';
const marker = 'MIMO_WG_POOL_OK';
const reports = [];
for (const api of ['chat/completions', 'responses']) {
  const started = performance.now();
  const payload = api === 'responses'
    ? { model, input: `Reply with exactly ${marker}.`, stream: true, store: false, max_output_tokens: 128 }
    : { model, messages: [{ role: 'user', content: `Reply with exactly ${marker}.` }], stream: true, max_tokens: 128 };
  const response = await fetch(`http://127.0.0.1:10000/v1/${api}`, {
    method: 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(payload), signal: AbortSignal.timeout(180_000),
  });
  const headersMs = Math.round(performance.now() - started);
  assert.equal(response.status, 200, `${api}: HTTP ${response.status}`);
  let buffer = '', content = '', firstFrameMs, firstContentMs, finished = false, heartbeats = 0;
  const decoder = new TextDecoder();
  for await (const bytes of response.body) {
    buffer += decoder.decode(bytes, { stream: true }).replaceAll('\r\n', '\n');
    let split;
    while ((split = buffer.indexOf('\n\n')) >= 0) {
      const frame = buffer.slice(0, split); buffer = buffer.slice(split + 2);
      if (frame.startsWith(':')) { heartbeats++; continue; }
      const data = frame.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n');
      if (!data) continue;
      firstFrameMs ??= Math.round(performance.now() - started);
      if (data === '[DONE]') { finished = true; continue; }
      const event = JSON.parse(data);
      assert(!event.error && event.type !== 'response.failed', `${api}: streamed generation failed`);
      const text = api === 'responses' ? event.type === 'response.output_text.delta' ? event.delta : '' : event.choices?.[0]?.delta?.content;
      if (text) { firstContentMs ??= Math.round(performance.now() - started); content += text; }
      if (event.type === 'response.completed' || event.type === 'response.incomplete') finished = true;
    }
  }
  assert(finished, `${api}: missing final event`);
  assert.match(content, new RegExp(marker));
  reports.push({ api, status: response.status, headers_ms: headersMs, first_frame_ms: firstFrameMs,
    first_content_ms: firstContentMs, completion_ms: Math.round(performance.now() - started), heartbeats, content });
  await writeFile('.state/server/mimo-pool-result.json', JSON.stringify({ tested_at: new Date().toISOString(), model, completed: false, reports }, null, 2));
  console.log(`${api}: headers ${headersMs}ms, text ${firstContentMs}ms, complete`);
}
await writeFile('.state/server/mimo-pool-result.json', JSON.stringify({ tested_at: new Date().toISOString(), model, completed: true, reports }, null, 2));
