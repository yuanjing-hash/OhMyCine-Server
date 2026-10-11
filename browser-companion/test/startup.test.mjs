import test from 'node:test';
import assert from 'node:assert/strict';
import { access, stat } from 'node:fs/promises';
import path from 'node:path';
import { openBrowser } from '../src/browser.mjs';
import { browserEnvironment, startupFailure } from '../src/startup.mjs';

const privateText = '\nCookie=synthetic-secret https://private.invalid/?token=synthetic /private/profile';
const failures = [
  ['No usable sandbox!', 'browser_sandbox_unavailable'],
  ['Failed to move to new namespace: Operation not permitted', 'browser_sandbox_unavailable'],
  ['chrome_crashpad_handler: --database is required', 'browser_profile_unavailable'],
  ['error while loading shared libraries: libX11.so: cannot open shared object file', 'browser_dependencies_missing'],
  ['browserType.launch: Executable does not exist at /synthetic', 'browser_executable_unavailable'],
  ['browserType.launch: spawn /synthetic EACCES', 'browser_executable_unavailable'],
  ['browserType.launch: Timeout 30000ms exceeded.', 'browser_launch_timeout'],
  ['Operation not permitted', 'browser_launch_failed'],
  ['unexpected exit', 'browser_launch_failed'],
];

for (const [message, code] of failures) test(`launch returns only safe ${code}: ${message}`, async () => {
  let environment, proxyClosed = 0;
  await assert.rejects(openBrowser('/synthetic', 'https://example.com', {
    resolvePinnedAddress: async () => '8.8.8.8',
    pinnedProxy: async () => ({ url: 'http://127.0.0.1:12345', close: async () => { proxyClosed++; } }),
    chromium: { launch: async options => { environment = options.env; throw new Error(message + privateText); } },
  }), error => {
    assert.equal(error.code, code); assert.equal(error.message, code);
    assert.equal(error.stack.includes('synthetic-secret'), false); return true;
  });
  assert.equal(proxyClosed, 1);
  if (process.platform === 'linux') {
    assert.notEqual(environment.HOME, process.env.HOME);
    await assert.rejects(access(environment.HOME));
  }
});

test('Linux private home is writable0700 and removed after context creation failure', { skip: process.platform !== 'linux' }, async () => {
  let environment, browserClosed = 0;
  await assert.rejects(openBrowser('/synthetic', 'https://example.com', {
    resolvePinnedAddress: async () => '8.8.8.8',
    pinnedProxy: async () => ({ url: 'http://127.0.0.1:12345', close: async () => {} }),
    chromium: { launch: async options => {
      environment = options.env;
      assert.equal((await stat(environment.HOME)).mode & 0o777, 0o700);
      assert.equal(environment.XDG_CONFIG_HOME, path.join(environment.HOME, 'config'));
      assert.equal(environment.XDG_CACHE_HOME, path.join(environment.HOME, 'cache'));
      assert.equal((await stat(environment.XDG_CONFIG_HOME)).mode & 0o777, 0o700);
      return { close: async () => { browserClosed++; }, newContext: async () => { throw new Error('synthetic context failure'); } };
    } },
  }));
  assert.equal(browserClosed, 1);
  await assert.rejects(access(environment.HOME));
});

test('private home preparation is isolated and idempotently removed', async () => {
  const first = await browserEnvironment('linux'), second = await browserEnvironment('linux');
  try {
    assert.notEqual(first.env.HOME, second.env.HOME);
    assert.notEqual(first.env.HOME, process.env.HOME);
    await access(first.env.XDG_CONFIG_HOME); await access(first.env.XDG_CACHE_HOME);
    assert.equal(first.env.HTTPS_PROXY, undefined); assert.equal(first.env.NODE_OPTIONS, undefined);
    await first.close(); await first.close();
    await assert.rejects(access(first.env.HOME)); await access(second.env.HOME);
  } finally { await first.close(); await second.close(); }
});

test('import codes, timeout type and hostile unbounded objects fail safely', () => {
  assert.equal(startupFailure({ code: 'ERR_MODULE_NOT_FOUND', message: privateText }).code, 'browser_dependencies_missing');
  assert.equal(startupFailure({ code: 'EACCES', message: privateText }).code, 'browser_executable_unavailable');
  assert.equal(startupFailure({ name: 'TimeoutError', message: privateText }).code, 'browser_launch_timeout');
  assert.equal(startupFailure({ message: 'x'.repeat(65536) + 'No usable sandbox!' }).code, 'browser_launch_failed');
  assert.equal(startupFailure({ get message() { throw new Error(privateText); } }).code, 'browser_launch_failed');
});

test('cancelled late launch closes browser and proxy before publishing a context', async () => {
  const controller = new AbortController();
  let finish, started, environment, browserClosed = 0, proxyClosed = 0;
  const entered = new Promise(resolve => { started = resolve; });
  const pending = openBrowser('/synthetic', 'https://example.com', {
    signal: controller.signal, resolvePinnedAddress: async () => '8.8.8.8',
    pinnedProxy: async () => ({ url: 'http://127.0.0.1:12345', close: async () => { proxyClosed++; } }),
    chromium: { launch: options => { environment = options.env; started(); return new Promise(resolve => { finish = resolve; }); } },
  });
  await entered; controller.abort();
  finish({ close: async () => { browserClosed++; }, newContext: async () => { assert.fail('cancelled context created'); } });
  await assert.rejects(pending, /^Error: request_cancelled$/);
  assert.equal(browserClosed, 1); assert.equal(proxyClosed, 1);
  if (process.platform === 'linux') await assert.rejects(access(environment.HOME));
});

async function requestLifecycleFixture({ closeFailure = false } = {}) {
  const calls = { browserClosed: 0, proxyClosed: 0 };
  const page = { on() {}, url: () => 'https://example.com/', goto: async () => {},
    // Untrusted page code may replace fetch/timers and never settle this work.
    evaluate: () => new Promise(() => {}) };
  const context = { setDefaultTimeout() {}, setDefaultNavigationTimeout() {}, on() {},
    route: async () => {}, routeWebSocket: async () => {}, newPage: async () => page };
  const adapter = await openBrowser('/synthetic', 'https://example.com', {
    resolvePinnedAddress: async () => '8.8.8.8',
    pinnedProxy: async () => ({ url: 'http://127.0.0.1:12345', close: async () => { calls.proxyClosed++; } }),
    chromium: { launch: async options => {
      calls.environment = options.env;
      return { newContext: async () => context, close: async () => {
        calls.browserClosed++;
        if (closeFailure) throw new Error('synthetic browser close failure');
      } };
    } },
  });
  return { adapter, calls };
}

test('Host deadline closes a nonresponsive page and releases every resource once', { timeout: 25_000 }, async () => {
  const { adapter, calls } = await requestLifecycleFixture();
  try {
    await assert.rejects(adapter.request({ url: 'https://example.com/private', method: 'GET' }),
      error => error.code === 'browser_request_timeout' && error.status === 504);
    // The deadline begins cleanup asynchronously; close joins that same work.
    await adapter.close(); await adapter.close();
    assert.equal(calls.browserClosed, 1); assert.equal(calls.proxyClosed, 1);
    if (process.platform === 'linux') await assert.rejects(access(calls.environment.HOME));
  } finally { await adapter.close(); }
});

test('browser close failure still releases proxy and private home exactly once', async () => {
  const { adapter, calls } = await requestLifecycleFixture({ closeFailure: true });
  await assert.rejects(adapter.close(), /synthetic browser close failure/);
  await assert.rejects(adapter.close(), /synthetic browser close failure/);
  assert.equal(calls.browserClosed, 1); assert.equal(calls.proxyClosed, 1);
  if (process.platform === 'linux') await assert.rejects(access(calls.environment.HOME));
});
