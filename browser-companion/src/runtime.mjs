import { randomUUID } from 'node:crypto';
import { fork } from 'node:child_process';
import { access, mkdir, readFile, writeFile, realpath } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Fault, requireThat, originURL } from './policy.mjs';
import { openBrowser } from './browser.mjs';
import { validateCookies } from './cookies.mjs';

export const SESSION_TTL = 30 * 60_000;
export function supportedPlatform(platform = process.platform, arch = process.arch) {
  return (platform === 'win32' && arch === 'x64') || (platform === 'linux' && ['x64', 'arm64'].includes(arch));
}
export class Runtime {
  constructor({ stateDir, opener = openBrowser, clock = Date.now, installer, allowTUNFakeIP = false } = {}) {
    requireThat(typeof stateDir === 'string' && path.isAbsolute(stateDir), 'state_directory_required');
    this.stateDir = stateDir; this.opener = opener; this.clock = clock;
    this.installer = installer || (() => this.acquire());
    this.allowTUNFakeIP = allowTUNFakeIP === true;
    this.state = 'not_installed'; this.session = null; this.busy = false; this.stopping = false;
  }
  async initialize() {
    await mkdir(this.stateDir, { recursive: true, mode: 0o700 });
    try {
      const record = JSON.parse(await readFile(path.join(this.stateDir, 'installation.json'), 'utf8'));
      this.executablePath = await this.validateExecutable(record.executablePath);
      this.state = 'ready';
    } catch { this.state = 'not_installed'; }
  }
  async validateExecutable(candidate) {
    requireThat(typeof candidate === 'string' && path.isAbsolute(candidate), 'install_failed');
    const root = await realpath(path.join(this.stateDir, 'cloak'));
    const actual = await realpath(candidate);
    const relative = path.relative(root, actual);
    requireThat(relative && !relative.startsWith('..') && !path.isAbsolute(relative), 'install_failed');
    await access(actual);
    return actual;
  }
  status() { return { protocolVersion: 1, state: this.state, supported: supportedPlatform(),
    installed: !!this.executablePath, runtimeError: this.runtimeError || '', tunFakeIPEnabled: this.allowTUNFakeIP }; }
  install(input) {
    requireThat(input.licenseAccepted === true, 'license_acceptance_required');
    requireThat(supportedPlatform(), 'platform_unsupported');
    if (this.executablePath || this.state === 'installing') return this.status();
    this.state = 'installing';
    this.installPromise = (async () => {
      try {
        const candidate = await this.installer();
        this.executablePath = await this.validateExecutable(candidate);
        await writeFile(path.join(this.stateDir, 'installation.json'), JSON.stringify({
          executablePath: this.executablePath, wrapper: '0.5.10', licenseAcceptedAt: new Date().toISOString(),
        }), { mode: 0o600 });
        this.state = 'ready';
      } catch { this.state = 'install_failed'; }
    })();
    return this.status();
  }
  acquire() {
    // Do not inherit overrides, proxy credentials, DEBUG or entitlement keys.
    const env = {};
    for (const key of ['PATH', 'Path', 'SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'HOME', 'USERPROFILE'])
      if (process.env[key]) env[key] = process.env[key];
    env.CLOAKBROWSER_CACHE_DIR = path.join(this.stateDir, 'cloak');
    env.CLOAKBROWSER_AUTO_UPDATE = 'false';
    return new Promise((resolve, reject) => {
      const child = fork(fileURLToPath(new URL('./install-worker.mjs', import.meta.url)), [], {
        env, silent: true, windowsHide: true, timeout: 12 * 60_000,
      });
      this.installChild = child;
      // Drain but never persist or forward upstream output (may contain URLs).
      child.stdout.resume(); child.stderr.resume();
      let result;
      child.on('message', value => { if (typeof value?.executablePath === 'string') result = value.executablePath; });
      child.on('error', () => reject(new Fault('install_failed')));
      child.on('exit', code => {
        this.installChild = null;
        if (code === 0 && result) resolve(result); else reject(new Fault('install_failed'));
      });
    });
  }
  async expire() {
    if (this.session && this.clock() >= this.session.expiresAt) {
      const stale = this.session; this.session = null;
      await stale.browser.close();
    }
  }
  async dispatch(route, input, { signal } = {}) {
    requireThat(!this.stopping, 'browser_stopping', 503);
    requireThat(!signal?.aborted, 'request_cancelled', 503);
    requireThat(input && typeof input === 'object' && !Array.isArray(input));
    if (route === '/v1/status') return this.status();
    if (route === '/v1/install') return this.install(input);
    requireThat(!this.busy, 'browser_busy', 409);
    this.busy = true;
    try {
      await this.expire();
      if (route === '/v1/session/create') {
        requireThat(this.state === 'ready' || this.state === 'launch_failed', 'browser_not_ready', 503);
        requireThat(!this.session, 'browser_busy', 409);
        requireThat(typeof input.identity === 'string' && /^[A-Za-z0-9:._-]{1,256}$/.test(input.identity));
        const origin = originURL(input.origin);
        const cookies = validateCookies(input.cookies ?? [], origin);
        let browser;
        try { browser = await this.opener(this.executablePath, origin, { cookies, allowTUNFakeIP: this.allowTUNFakeIP }); }
        catch (error) {
          const codes = ['browser_launch_failed', 'browser_navigation_failed', 'network_denied', 'network_timeout', 'tun_fake_ip_requires_opt_in'];
          this.runtimeError = error instanceof Fault && codes.includes(error.code) ? error.code : 'browser_launch_failed';
          this.state = 'launch_failed';
          throw new Fault(this.runtimeError, 503);
        }
        this.state = 'ready'; this.runtimeError = '';
        if (this.stopping || signal?.aborted) {
          await browser.close();
          throw new Fault(this.stopping ? 'browser_stopping' : 'request_cancelled', 503);
        }
        const expiresAt = this.clock() + SESSION_TTL;
        this.session = { id: randomUUID(), identity: input.identity, origin, browser, expiresAt };
        return { sessionId: this.session.id, expiresAt: new Date(expiresAt).toISOString() };
      }
      const session = this.session;
      requireThat(session && input.sessionId === session.id && input.identity === session.identity,
        'session_unavailable', 404);
      switch (route) {
        case '/v1/session/snapshot': return await session.browser.snapshot();
        case '/v1/session/reload': return await session.browser.reload();
        case '/v1/session/input': return await session.browser.input(input);
        case '/v1/session/navigate': return await session.browser.navigate(input);
        case '/v1/session/request': return await session.browser.request(input);
        case '/v1/session/cookies': return { cookies: await session.browser.cookies() };
        case '/v1/session/close':
          this.session = null; await session.browser.close(); return {};
        default: throw new Fault('not_found', 404);
      }
    } finally { this.busy = false; }
  }
  async close() {
    this.stopping = true;
    this.installChild?.kill();
    const session = this.session; this.session = null;
    await session?.browser.close();
  }
}
