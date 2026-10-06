import { spawn } from 'node:child_process';
import { mkdir, access, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { randomBytes } from 'node:crypto';
import { createServer } from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';

export async function prepareProfile(directory) {
  const root = resolve(directory, 'opencode');
  const names = ['data', 'config', 'cache', 'state', 'temp', 'home', 'workspace'];
  for (const name of names) await mkdir(join(root, name), { recursive: true, mode: 0o700 });
  const configPath = join(root, 'config', 'opencode.json');
  await writeFile(configPath, JSON.stringify({ permission: { '*': 'ask' }, plugin: [], mcp: {} }));
  const env = {};
  const keep = /^(PATH|PATHEXT|SYSTEMROOT|WINDIR|SYSTEMDRIVE|COMSPEC|APPDATA|LOCALAPPDATA|USERPROFILE|PROGRAMFILES|PROGRAMFILES\(X86\)|PROGRAMDATA|LANG|LC_.+|SSL_CERT_FILE|SSL_CERT_DIR|HTTPS_PROXY|HTTP_PROXY|NO_PROXY)$/i;
  for (const [key, value] of Object.entries(process.env)) if (keep.test(key)) env[key] = value;
  Object.assign(env, {
    XDG_DATA_HOME: join(root, 'data'), XDG_CONFIG_HOME: join(root, 'config'),
    XDG_CACHE_HOME: join(root, 'cache'), XDG_STATE_HOME: join(root, 'state'),
    TMPDIR: join(root, 'temp'), TMP: join(root, 'temp'), TEMP: join(root, 'temp'),
    OPENCODE_CONFIG: configPath, OPENCODE_CONFIG_DIR: join(root, 'config'),
    OPENCODE_TEST_HOME: join(root, 'home'), OPENCODE_DISABLE_PROJECT_CONFIG: 'true',
    OPENCODE_SERVER_PASSWORD: randomBytes(32).toString('hex'), NO_COLOR: '1'
  });
  const executable = process.env.OPENCODE_BINARY || (process.platform === 'win32'
    ? join(process.env.APPDATA, 'npm', 'node_modules', 'opencode-ai', 'bin', 'opencode.exe') : 'opencode');
  if (process.platform === 'win32') await access(executable);
  return { root, env, executable };
}

export async function startRuntime(directory) {
  const { root, env, executable } = await prepareProfile(directory);
  const port = await new Promise((resolvePort, reject) => {
    const server = createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => { const port = server.address().port; server.close(() => resolvePort(port)); });
  });
  const child = spawn(executable, ['serve', '--hostname', '127.0.0.1', '--port', String(port), '--pure'], {
    cwd: join(root, 'workspace'), env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe']
  });
  let output = '';
  child.stdout.on('data', chunk => { output = (output + chunk).slice(-16000); });
  child.stderr.on('data', chunk => { output = (output + chunk).slice(-16000); });
  let spawnError;
  child.on('error', error => { spawnError = error; });
  const exited = new Promise(resolveExit => child.once('exit', (code, signal) => resolveExit({ code, signal })));
  const baseURL = `http://127.0.0.1:${port}`;
  const authorization = `Basic ${Buffer.from(`opencode:${env.OPENCODE_SERVER_PASSWORD}`).toString('base64')}`;
  const request = (path, options = {}) => fetch(baseURL + path, {
    ...options, signal: options.signal || AbortSignal.timeout(30000),
    headers: { Authorization: authorization, 'Content-Type': 'application/json', ...options.headers }
  });
  const stop = async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill();
    await Promise.race([exited, delay(5000, undefined, { ref: false })]);
    if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
  };
  try {
    for (let count = 0; count < 120; count++) {
      if (spawnError) throw spawnError;
      if (child.exitCode !== null) throw new Error(`OpenCode startup failed: ${output}`);
      try {
        const response = await request('/global/health', { signal: AbortSignal.timeout(1000) });
        if (response.ok) return { baseURL, authorization, root, request, stop, pid: child.pid, env };
      } catch {}
      await delay(500);
    }
    throw new Error(`OpenCode startup timeout: ${output}`);
  } catch (error) { await stop(); throw error; }
}
