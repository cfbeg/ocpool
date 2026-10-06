import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import plugin from '../internal/app/parameters.mjs';

test('reasoning uses native provider variants; unavailable variants retain model defaults', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'ocpool-parameters-'));
  const old = process.env.OCPOOL_PARAMS_DIR;
  process.env.OCPOOL_PARAMS_DIR = directory;
  try {
    const hooks = await plugin();
    for (const [variants, expected, mode] of [
      [{ high: { reasoningEffort: 'high' } }, { reasoningEffort: 'high' }, 'variant'],
      [{ high: { thinking: { type: 'enabled', budgetTokens: 16000 } } }, { thinking: { type: 'enabled', budgetTokens: 16000 } }, 'variant'],
      [{}, {}, 'model_default'],
    ]) {
      await writeFile(join(directory, 'ses_test.json'), JSON.stringify({ reasoning: { effort: 'high' }, max_output_tokens: 500 }));
      const output = { options: {} };
      await hooks['chat.params']({ sessionID: 'ses_test', agent: 'build', model: { variants, limit: { output: 128 } } }, output);
      assert.deepEqual(output.options, expected);
      assert.equal(output.maxOutputTokens, 128);
      const ack = JSON.parse(await readFile(join(directory, 'ses_test.applied.json'), 'utf8'));
      assert.equal(ack.reasoning_mode, mode);
      assert.equal(ack.reasoning.effort, 'high');
    }
  } finally {
    if (old === undefined) delete process.env.OCPOOL_PARAMS_DIR; else process.env.OCPOOL_PARAMS_DIR = old;
    await rm(directory, { recursive: true, force: true });
  }
});

test('egress fetch preserves RequestInfo fallback bodies and native Zen payload', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'ocpool-egress-plugin-'));
  const previousDirectory = process.env.OCPOOL_PARAMS_DIR;
  const previousGateway = process.env.OCPOOL_EGRESS_URL;
  const previousKey = process.env.OCPOOL_EGRESS_TOKEN;
  const originalFetch = globalThis.fetch;
  process.env.OCPOOL_PARAMS_DIR = directory;
  process.env.OCPOOL_EGRESS_URL = 'http://127.0.0.1:18000';
  process.env.OCPOOL_EGRESS_TOKEN = 'private-fixture';
  try {
    const seen = [];
    globalThis.fetch = async (input, init) => {
      const req = new Request(input, init);
      seen.push({ url: req.url, headers: req.headers, text: await req.text() });
      return new Response('fixture');
    };
    const hooks = await plugin(); const config = {};
    await hooks.config(config);
    const forward = config.provider.opencode.options.fetch;
    const originalBody = '{"messages":[{"role":"user","content":"hello"}],"tool_choice":"auto"}';
    await forward(new Request('https://example.com/api', { method: 'POST', body: originalBody }));
    assert.equal(seen[0].url, 'https://example.com/api'); assert.equal(seen[0].text, originalBody);
    await forward('https://opencode.ai/zen/v1/responses', { method: 'POST', body: originalBody, headers: { Authorization: 'Bearer fixture-upstream' } });
    assert.equal(seen[1].url, 'http://127.0.0.1:18000/'); assert.equal(seen[1].text, originalBody);
    assert.equal(seen[1].headers.get('x-ocpool-target'), 'https://opencode.ai/zen/v1/responses');
    assert.equal(seen[1].headers.get('authorization'), 'Bearer fixture-upstream');
    await forward('https://opencode.ai/inference/openai/v1/responses', { method: 'POST', body: originalBody, headers: { Authorization: 'Bearer fixture-console', 'x-opencode-org-id': 'org_fixture' } });
    assert.equal(seen[2].url, 'http://127.0.0.1:18000/'); assert.equal(seen[2].text, originalBody);
    assert.equal(seen[2].headers.get('x-ocpool-target'), 'https://opencode.ai/inference/openai/v1/responses');
    assert.equal(seen[2].headers.get('x-opencode-org-id'), 'org_fixture');
  } finally {
    globalThis.fetch = originalFetch;
    for (const [name, value] of [['OCPOOL_PARAMS_DIR', previousDirectory], ['OCPOOL_EGRESS_URL', previousGateway], ['OCPOOL_EGRESS_TOKEN', previousKey]]) {
      if (value === undefined) delete process.env[name]; else process.env[name] = value;
    }
    await rm(directory, { recursive: true, force: true });
  }
});
