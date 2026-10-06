import { startRuntime } from './opencode-runtime.mjs';
const runtime = await startRuntime('.state/research/runtime-probe');
try {
  for (const path of ['/global/health', '/api/integration/opencode', '/provider/auth']) {
    const response = await runtime.request(path);
    console.log(JSON.stringify({ path, status: response.status, body: await response.json().catch(() => 'non_json') }));
  }
} finally { await runtime.stop(); }
