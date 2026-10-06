import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { prepareProfile } from './opencode-runtime.mjs';
const directory = resolve(process.argv[2] || '.state/onboarding/live-test-001');
const profile = await prepareProfile(directory);
const execute = promisify(execFile);
const options = { cwd: join(profile.root, 'workspace'), env: profile.env, windowsHide: true, timeout: 150000, maxBuffer: 4 * 1024 * 1024 };
const orgs = await execute(profile.executable, ['console', 'orgs', '--pure'], options);
console.log(JSON.stringify({ step: 'console_orgs', output: `${orgs.stdout}\n${orgs.stderr}`.replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '').trim() }));
let result;
try {
  const pending = execute(profile.executable, ['run', '--model', 'opencode/big-pickle', '--format', 'json', '--pure', 'Reply with exactly OK.'], options);
  pending.child.stdin?.end();
  result = await pending;
}
catch (error) { result = { stdout: error.stdout || '', stderr: error.stderr || '', code: error.code }; }
await writeFile(join(directory, 'native-probe.log'), `${result.stdout}\n${result.stderr}`, { mode: 0o600 });
const summaries = [];
for (const line of result.stdout.split(/\r?\n/)) {
  try {
    const event = JSON.parse(line);
    if (event.type === 'text') summaries.push({ type: event.type, text: event.part?.text });
    if (event.type === 'error') summaries.push({ type: event.type, name: event.error?.name, message: event.error?.data?.message });
  } catch {}
}
console.log(JSON.stringify({ step: 'native_run', code: result.code || 0, events: summaries }, null, 2));
