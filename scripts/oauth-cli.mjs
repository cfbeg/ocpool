import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { startRuntime } from './opencode-runtime.mjs';
import { authorizeHeadless } from './oauth-headless.mjs';

const execute = promisify(execFile);
export const authorizeCLI = authorizeHeadless;

export async function probeAccount(directory, { prompt = false, model: requestedModel } = {}) {
  const runtime = await startRuntime(directory);
  const connectionPath = resolve(directory, 'runtime-connection.json');
  try {
    await writeFile(connectionPath, JSON.stringify({ url: runtime.baseURL, authorization: runtime.authorization,
      directory: join(runtime.root, 'workspace') }), { mode: 0o600 });
    const executable = resolve('bin', process.platform === 'win32' ? 'opencode-probe.exe' : 'opencode-probe');
    const invoke = async extra => {
      const result = await execute(executable, ['--connection', connectionPath, ...extra], {
        windowsHide: true, timeout: 160000, maxBuffer: 1024 * 1024
      });
      return JSON.parse(result.stdout);
    };
    const catalog = await invoke([]);
    if (!prompt) return catalog;
    const models = catalog.zero_cost_catalog_models || [];
    const model = requestedModel || (models.includes('opencode/big-pickle') ? 'opencode/big-pickle' : models.find(id => id.endsWith('-free')));
    if (!model) return { ...catalog, inference: 'no_matching_free_model' };
    return await invoke(['--model', model, '--prompt', 'Reply with exactly OK.']);
  } finally { await runtime.stop(); }
}
