import { launchPersistentContext } from 'cloakbrowser';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';

export async function launchJobBrowser(directory, { headed = false } = {}) {
  const userDataDir = resolve(directory, 'browser-cloak');
  const defaultProfile = resolve(userDataDir, 'Default');
  await mkdir(defaultProfile, { recursive: true, mode: 0o700 });
  {
    const preferencesPath = resolve(defaultProfile, 'Preferences');
    let preferences = {};
    try { preferences = JSON.parse(await readFile(preferencesPath, 'utf8')); }
    catch (error) { if (error.code !== 'ENOENT') throw error; }
    const zoom = headed ? Number(process.env.OCPOOL_BROWSER_ZOOM || 0.67) : 1;
    if (!Number.isFinite(zoom) || zoom < 0.5 || zoom > 1.5) throw new Error('Invalid OCPOOL_BROWSER_ZOOM');
    preferences.partition ??= {};
    preferences.partition.default_zoom_level ??= {};
    preferences.partition.default_zoom_level.x = Math.log(zoom) / Math.log(1.2);
    await writeFile(preferencesPath, JSON.stringify(preferences), { mode: 0o600 });
  }
  // Chromium refuses to start with its sandbox enabled when the container
  // runs the worker as UID 0. Keep the sandbox for normal users, but make the
  // default headless Docker deployment usable without requiring an X server or
  // a custom wrapper script.
  const rootArgs = process.platform === 'linux' && typeof process.getuid === 'function' && process.getuid() === 0
    ? ['--no-sandbox', '--disable-setuid-sandbox'] : [];
  return launchPersistentContext({
    userDataDir,
    headless: !headed,
    browserVersion: process.env.OCPOOL_BROWSER_VERSION || '146.0.7680.177.5',
    viewport: headed ? null : { width: 1280, height: 720 },
    args: [...rootArgs, ...(headed ? ['--start-maximized', '--window-position=0,0'] : [])]
  });
}
