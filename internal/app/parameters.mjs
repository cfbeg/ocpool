import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

// The gateway owns this directory and writes one file per upstream session.
export default async function OCPoolParameters() {
  const directory = process.env.OCPOOL_PARAMS_DIR;
  if (!directory) throw new Error('OCPool parameter directory is missing');
  await writeFile(join(directory, 'ready.json'), JSON.stringify({ run: process.env.OCPOOL_PARAMS_RUN, version: 1 }));
  return {
    config: async config => {
      if (process.env.OCPOOL_SMALL_MODEL) config.small_model = process.env.OCPOOL_SMALL_MODEL;
      const gateway = process.env.OCPOOL_EGRESS_URL;
      if (!gateway) return;
      config.provider ??= {};
      config.provider.opencode ??= {};
      config.provider.opencode.options ??= {};
      const previous = config.provider.opencode.options.fetch || fetch;
      config.provider.opencode.options.fetch = async (input, init) => {
        const target = new URL(input instanceof Request ? input.url : input);
        const inferencePath = /^\/inference\/(?:openai\/v1|anthropic\/v1|google\/v1beta)\//.test(target.pathname);
        if (target.origin !== 'https://opencode.ai' || (!/^\/zen\/(?:go\/)?v1\//.test(target.pathname) && !inferencePath)) return previous(input, init);
        const request = new Request(input, init);
        const headers = new Headers(request.headers);
        headers.set('x-ocpool-egress-auth', process.env.OCPOOL_EGRESS_TOKEN);
        headers.set('x-ocpool-target', target.href);
        headers.set('x-ocpool-method', request.method);
        return fetch(gateway, {
          method: 'POST', headers, body: ['GET', 'HEAD'].includes(request.method) ? undefined : await request.arrayBuffer(),
          signal: request.signal,
        });
      };
    },
    'chat.headers': async (input, output) => {
      if (process.env.OCPOOL_EGRESS_URL) output.headers['x-ocpool-agent'] = input.agent;
    },
    'chat.params': async (input, output) => {
      if (input.agent !== 'build' || !/^[a-zA-Z0-9_-]+$/.test(input.sessionID)) return;
      let options;
      try { options = JSON.parse(await readFile(join(directory, `${input.sessionID}.json`), 'utf8')); }
      catch (error) { if (error.code === 'ENOENT') return; throw error; }
      if (options.temperature !== undefined) output.temperature = options.temperature;
      if (options.top_p !== undefined) output.topP = options.top_p;
      if (options.max_output_tokens !== undefined) {
        const limit = input.model?.limit?.output;
        output.maxOutputTokens = Number.isFinite(limit) && limit > 0
          ? Math.min(limit, options.max_output_tokens) : options.max_output_tokens;
      }
      let reasoningMode;
      if (options.reasoning) {
        const { effort, summary } = options.reasoning;
        const variant = input.model?.variants?.[effort];
        // OpenCode's model catalog supplies provider-specific thinking budgets,
        // effort values and switches. Models without variants retain their default.
        reasoningMode = variant ? 'variant' : 'model_default';
        if (variant) Object.assign(output.options, structuredClone(variant));
        if (summary && ['@ai-sdk/openai', '@ai-sdk/azure'].includes(input.model?.api?.npm)) {
          output.options.reasoningSummary = summary;
        }
      }
      await writeFile(join(directory, `${input.sessionID}.applied.json`), JSON.stringify({
        temperature: output.temperature, top_p: output.topP, max_output_tokens: output.maxOutputTokens,
        reasoning: options.reasoning, reasoning_mode: reasoningMode
      }));
    }
  };
}
