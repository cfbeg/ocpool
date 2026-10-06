import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

// Use the same installed provider and parser as the reporting Pi client.
const root = process.env.OCPOOL_PI_AI_DIR || join(process.env.APPDATA, 'npm/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/dist');
const { stream: responses } = await import(pathToFileURL(join(root, 'api/openai-responses.js')));
const { stream: completions } = await import(pathToFileURL(join(root, 'api/openai-completions.js')));
const apiKey = (await readFile('.state/server/admin-key.txt', 'utf8')).trim();
const baseModel = {
  id: process.env.OCPOOL_SMOKE_MODEL || 'opencode/big-pickle', name: 'OCPool Test Model', provider: 'ocpool',
  baseUrl: 'http://127.0.0.1:10000/v1', reasoning: true, input: ['text'],
  contextWindow: 128000, maxTokens: 4096,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
};
const tool = { name: 'lookup_code', description: 'Look up the public test code for a city. The code is returned by the client.',
  parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'], additionalProperties: false } };
const system = { role: 'system', content: 'Follow the user request exactly.', timestamp: Date.now() };
const user = content => ({ role: 'user', content, timestamp: Date.now() });
const reports = [];
const autoTools = process.argv.includes('--auto-tools');
const codingTools = autoTools || process.argv.includes('--coding-tools');
async function run(api, messages, extra = {}) {
  const model = { ...baseModel, api };
  const request = (api === 'openai-responses' ? responses : completions)(model, { messages }, {
    apiKey, reasoningEffort: process.env.OCPOOL_SMOKE_EFFORT || 'low', maxTokens: 2048, sessionId: 'ocpool-pi-smoke',
    signal: AbortSignal.timeout(180000), ...extra,
  });
  const events = [];
  for await (const event of request) events.push(event.type);
  const result = await request.result();
  assert.notEqual(result.stopReason, 'error', result.errorMessage);
  assert.notEqual(result.stopReason, 'aborted', result.errorMessage);
  assert(events.includes('done'), events.join(','));
  reports.push({ api, stop: result.stopReason, events: [...new Set(events)], usage: result.usage,
    tool_calls: result.content.filter(x => x.type === 'toolCall').map(x => ({ id: x.id, name: x.name, arguments: x.arguments })) });
  await writeFile(`.state/server/${autoTools ? 'pi-auto-tools-result' : codingTools ? 'pi-coding-tools-result' : 'pi-smoke-result'}.json`, JSON.stringify({ tested_at: new Date().toISOString(), completed: false, model: baseModel.id, reports }, null, 2));
  return result;
}
for (const api of ['openai-responses', 'openai-completions']) {
  if (codingTools) {
    const tools = [
      { name: 'read', description: 'Read a text file in the client workspace.', parameters: {
        type: 'object', properties: { path: { type: 'string' }, offset: { type: 'integer' }, limit: { type: 'integer' } }, required: ['path'], additionalProperties: false,
      } },
      { name: 'bash', description: 'Execute a shell command in the client workspace.', parameters: {
        type: 'object', properties: { command: { type: 'string' } }, required: ['command'], additionalProperties: false,
      } },
    ];
    const messages = [{ ...system, toolsAdded: tools }, user('Read client-only/fixture.txt using the read tool and report the public test code in it.')];
    const first = await run(api, messages, {
      toolChoice: autoTools ? 'auto' : api === 'openai-responses' ? { type: 'function', name: 'read' } : { type: 'function', function: { name: 'read' } },
      onPayload: params => ({ ...params, parallel_tool_calls: false }),
    });
    const calls = first.content.filter(x => x.type === 'toolCall');
    assert.equal(first.stopReason, 'toolUse');
    assert.equal(calls.length, 1);
    assert.equal(calls[0].name, 'read');
    assert.equal(calls[0].arguments.path, 'client-only/fixture.txt', `${api} read arguments: ${JSON.stringify(calls[0].arguments)}`);
    console.log(`${api}: ${autoTools ? 'auto' : 'named'} read tool and parallel_tool_calls=false OK`);
    const second = await run(api, [...messages, first, {
      role: 'toolResult', toolCallId: calls[0].id, toolName: 'read',
      content: [{ type: 'text', text: 'Public test code: CLIENT_READ_OK_492' }],
      isError: false, timestamp: Date.now(),
    }]);
    assert.equal(second.stopReason, 'stop');
    assert.match(second.content.filter(x => x.type === 'text').map(x => x.text).join(''), /CLIENT_READ_OK_492/);
    console.log(`${api}: client read result replay OK`);
    continue;
  }
  const simple = await run(api, [system, user('Reply with exactly PI_REASONING_OK.')]);
  assert.match(simple.content.filter(x => x.type === 'text').map(x => x.text).join(''), /PI_REASONING_OK/);
  console.log(`${api}: reasoning and text OK`);
  const messages = [{ ...system, toolsAdded: [tool] }, user('Call lookup_code with city Tokyo and tell me the public test code returned by the tool.')];
  const first = await run(api, messages, { toolChoice: 'required' });
  const call = first.content.find(x => x.type === 'toolCall');
  assert.equal(first.stopReason, 'toolUse', JSON.stringify(first.content));
  assert.equal(call?.name, 'lookup_code');
  assert.equal(call.arguments.city, 'Tokyo');
  console.log(`${api}: tool call parsed by Pi OK`);
  const second = await run(api, [...messages, first, {
    role: 'toolResult', toolCallId: call.id, toolName: call.name,
    content: [{ type: 'text', text: '{"city":"Tokyo","code":"PI_TOOL_RESULT_739"}' }],
    isError: false, timestamp: Date.now(),
  }]);
  assert.equal(second.stopReason, 'stop');
  assert.match(second.content.filter(x => x.type === 'text').map(x => x.text).join(''), /PI_TOOL_RESULT_739/);
  console.log(`${api}: reasoning + function history replay OK`);
}
await writeFile(`.state/server/${autoTools ? 'pi-auto-tools-result' : codingTools ? 'pi-coding-tools-result' : 'pi-smoke-result'}.json`, JSON.stringify({ tested_at: new Date().toISOString(), completed: true, model: baseModel.id, reports }, null, 2));
