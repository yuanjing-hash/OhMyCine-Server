import { mkdtemp, mkdir, chmod, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { Fault } from './policy.mjs';

export const STARTUP_CODES = Object.freeze(['browser_launch_failed', 'browser_sandbox_unavailable',
  'browser_dependencies_missing', 'browser_executable_unavailable', 'browser_launch_timeout', 'browser_profile_unavailable']);

// Classify bounded, transient launch/import text only. Do not retain causes,
// return logs, log the exception, or treat a generic EPERM as a sandbox failure.
export function startupFailure(error) {
  let message = '', code = '', name = '';
  try {
    if (typeof error?.message === 'string') message = error.message.slice(0, 65536);
    if (typeof error?.code === 'string') code = error.code;
    if (typeof error?.name === 'string') name = error.name;
  } catch { /* Unknown/untrusted values remain generic. */ }
  let safe = 'browser_launch_failed';
  if (/No usable sandbox!|Failed to move to new namespace[^\n]{0,512}Operation not permitted|Failed to unshare[^\n]{0,512}Operation not permitted/i.test(message))
    safe = 'browser_sandbox_unavailable';
  else if (/chrome_crashpad_handler:\s*--database is required|crashpad[^\n]{0,256}(?:Permission denied|Read-only file system)/i.test(message))
    safe = 'browser_profile_unavailable';
  else if (['ERR_MODULE_NOT_FOUND', 'MODULE_NOT_FOUND'].includes(code) || /error while loading shared libraries:|Host system is missing dependencies to run browsers|Cannot find (?:package|module) ['"]playwright-core/i.test(message))
    safe = 'browser_dependencies_missing';
  else if (['EACCES', 'ENOENT', 'ENOEXEC'].includes(code) || /Executable doesn't exist|Executable does not exist|spawn[^\n]{0,1024}\b(?:EACCES|ENOENT|ENOEXEC)\b/i.test(message))
    safe = 'browser_executable_unavailable';
  else if (name === 'TimeoutError' || /browserType\.launch:\s*Timeout \d+ms exceeded/i.test(message))
    safe = 'browser_launch_timeout';
  return new Fault(safe, 503);
}

export async function browserEnvironment(platform = process.platform) {
  const env = {};
  for (const key of ['PATH', 'Path', 'SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'HOME', 'USERPROFILE',
    'LOCALAPPDATA', 'APPDATA', 'LANG', 'LC_ALL', 'DISPLAY', 'XDG_RUNTIME_DIR'])
    if (process.env[key]) env[key] = process.env[key];
  if (platform !== 'linux') return { env, close: async () => {} };
  // A numeric container UID need not have a passwd home. Chromium Crashpad
  // still requires writable config paths even with an ephemeral Playwright
  // profile. Never adopt the Server operator's browser/home configuration.
  let home;
  try {
    home = await mkdtemp(path.join(tmpdir(), 'omc-browser-home-'));
    await chmod(home, 0o700);
    env.HOME = home;
    env.XDG_CONFIG_HOME = path.join(home, 'config');
    env.XDG_CACHE_HOME = path.join(home, 'cache');
    await mkdir(env.XDG_CONFIG_HOME, { mode: 0o700 });
    await mkdir(env.XDG_CACHE_HOME, { mode: 0o700 });
  } catch {
    if (home) await rm(home, { recursive: true, force: true }).catch(() => {});
    throw new Fault('browser_profile_unavailable', 503);
  }
  let closed;
  return { env, close() {
    return closed ??= rm(home, { recursive: true, force: true, maxRetries: 2, retryDelay: 50 })
      .catch(() => { throw new Fault('browser_profile_unavailable', 503); });
  } };
}
