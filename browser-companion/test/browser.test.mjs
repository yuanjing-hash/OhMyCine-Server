import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { openBrowser } from '../src/browser.mjs';
import { pinnedProxy } from '../src/proxy.mjs';
import { validateCookies, exportCookies } from '../src/cookies.mjs';

async function fixture(cookies = [], network = {}) {
  const calls = {};
  const page = {
    url: () => calls.pageURL || 'https://example.com/',
    on: () => {},
    goto: async url => { calls.navigatedWithCookies = calls.importedCookies; calls.navigation = url; },
    reload: async () => { calls.reloads = (calls.reloads || 0) + 1; },
    screenshot: async () => Buffer.from('synthetic'),
    mouse: { click: async (...v) => { calls.click = v; }, wheel: async () => {} },
    keyboard: { insertText: async () => {}, press: async () => {} },
    evaluate: async (fn, input) => {
      calls.script = fn.toString(); calls.request = input;
      if (calls.execute) return await fn(input);
      return calls.result || { status: 200, headers: { 'content-type': 'application/json', 'set-cookie': 'must-drop' }, bodyBase64: 'e30=' };
    },
  };
  const context = { setDefaultTimeout() {}, setDefaultNavigationTimeout() {}, on() {}, cookies: async () => calls.cookies || [],
    addCookies: async value => { calls.importedCookies = value; },
    route: async (_, fn) => { calls.route = fn; },
    routeWebSocket: async (_, fn) => { calls.socket = fn; }, newPage: async () => page };
  const browser = { newContext: async options => { calls.context = options; return context; }, close: async () => {} };
  const adapter = await openBrowser('/synthetic', 'https://example.com', {
    cookies,
    allowTUNFakeIP: network.allowTUNFakeIP,
    resolvePublic: async (origin, resolver, allowed) => { calls.resolution = { origin, allowed }; return network.address || '8.8.8.8'; },
    pinnedProxy: async (origin, address) => { calls.pinned = { origin, address }; return { url: 'http://127.0.0.1:12345', close: async () => {} }; },
    chromium: { launch: async options => { calls.launch = options; return browser; } },
  });
  return { adapter, calls };
}
test('browser retains sandbox, constrained proxy, no downloads/SW/WebSockets', async () => {
  const { calls, adapter } = await fixture();
  assert.equal(calls.launch.chromiumSandbox, true);
  assert.equal(calls.launch.env.OMC_CLOAK_TOKEN, undefined);
  assert.equal(calls.launch.args.includes('--no-sandbox'), false);
  assert.equal(calls.context.acceptDownloads, false);
  assert.equal(calls.context.serviceWorkers, 'block');
  let denied = false, allowed = false;
  await calls.route({ request: () => ({ url: () => 'https://127.0.0.1/' }), abort: () => { denied = true; } });
  await calls.route({ request: () => ({ url: () => 'https://example.com/path' }), continue: () => { allowed = true; } });
  assert.equal(denied, true); assert.equal(allowed, true);
  let closed = false; calls.socket({ close: () => { closed = true; } }); assert.equal(closed, true);
  await adapter.close();
});

test('TUN routing retains numeric pinning, original HTTPS origin and certificate checks', async () => {
  const { calls, adapter } = await fixture([], { allowTUNFakeIP: true, address: '198.18.7.137' });
  assert.deepEqual(calls.resolution, { origin: 'https://example.com', allowed: true });
  assert.deepEqual(calls.pinned, { origin: 'https://example.com', address: '198.18.7.137' });
  assert.equal(calls.navigation, 'https://example.com');
  assert.equal(calls.context.ignoreHTTPSErrors, false);
  assert.equal(calls.launch.args.some(arg => arg.includes('ignore-certificate-errors')), false);
  await adapter.close();
});

test('public resources and child frames allowed while top-level redirects remain fixed', async () => {
  const { adapter, calls } = await fixture();
  async function permitted(url, main = false) {
    let allowed = false;
    await calls.route({ request: () => ({ url: () => url, isNavigationRequest: () => main,
      frame: () => ({ parentFrame: () => null }), method: () => 'GET' }),
      continue: () => { allowed = true; }, abort: () => {} });
    return allowed;
  }
  assert.equal(await permitted('https://cdn.example/script.js'), true);
  let childAllowed = false;
  await calls.route({ request: () => ({ url: () => 'https://frames.example/widget', isNavigationRequest: () => true,
    frame: () => ({ parentFrame: () => ({}) }) }), continue: () => { childAllowed = true; }, abort: () => {} });
  assert.equal(childAllowed, true);
  assert.equal(await permitted('https://other.example/', true), false);
  assert.equal(await permitted('https://example.com/login', true), true);
  for (const url of ['https://127.0.0.1/', 'https://cdn.example:444/', 'http://cdn.example/', 'file:///tmp/x'])
    assert.equal(await permitted(url), false);
  assert.equal((await adapter.snapshot()).blockedResourceCount, 5);
  await adapter.reload();
  assert.equal(calls.reloads, 1);
  assert.equal((await adapter.snapshot()).blockedResourceCount, 0);
  assert.equal((await adapter.snapshot()).networkErrorCode, '');
  calls.pageURL = 'https://example.com/#login';
  await adapter.snapshot();
  await adapter.reload();
  assert.equal(calls.reloads, 2);
  await calls.route({ request: () => ({ url: () => 'https://example.com/login', isNavigationRequest: () => true,
    frame: () => ({ parentFrame: () => null }), method: () => 'POST' }), continue: () => {}, abort: () => {} });
  await assert.rejects(adapter.reload(), /browser_reload_post_denied/);
  assert.equal(calls.reloads, 2);
  await adapter.close();
});

const sessionCookie = { name: 'session', value: 'synthetic', domain: 'example.com', path: '/',
  expires: -1, httpOnly: true, secure: true, sameSite: 'Lax' };
test('cookie state restores before navigation and exports only narrow validated fields', async () => {
  const { adapter, calls } = await fixture([{ ...sessionCookie, ignored: 'never-persist' }]);
  assert.deepEqual(calls.navigatedWithCookies, [sessionCookie]);
  calls.cookies = [{ ...sessionCookie, domain: '.example.com', extra: 'never-persist' },
    { ...sessionCookie, domain: 'foreign.example' }];
  assert.deepEqual(await adapter.cookies(), [sessionCookie]);
  assert.deepEqual((await adapter.request({ url: 'https://example.com/', method: 'GET' })).cookies, [sessionCookie]);
  const childOrigin = 'https://www.example.com';
  assert.deepEqual(await exportCookies({ cookies: async () => [{ ...sessionCookie, domain: '.example.com' }] }, childOrigin),
    [{ ...sessionCookie, domain: 'www.example.com' }]);
});
test('cookie import rejects foreign domain, injected values, duplicate identity and oversized state', () => {
  for (const cookies of [[{ ...sessionCookie, domain: '.example.com' }], [{ ...sessionCookie, domain: 'foreign.example' }],
    [{ ...sessionCookie, value: 'x\r\nInjected: yes' }], [{ ...sessionCookie, path: '/; HttpOnly' }],
    [{ ...sessionCookie, expires: Infinity }], [sessionCookie, sessionCookie], Array(129).fill(sessionCookie),
    [{ ...sessionCookie, value: 'x'.repeat(4097) }]])
    assert.throws(() => validateCookies(cookies, 'https://example.com'));
});
test('same-context response preserves non-UTF8 captcha bytes exactly', async t => {
  const { adapter, calls } = await fixture();
  const png = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10, 0, 255, 128, 200]);
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  globalThis.fetch = async () => new Response(png, { headers: { 'Content-Type': 'image/png' } });
  calls.execute = true;
  const result = await adapter.request({ url: 'https://example.com/captcha', method: 'GET' });
  assert.deepEqual(Buffer.from(result.bodyBase64, 'base64'), png);
  assert.equal(result.headers['content-type'], 'image/png');
  assert.equal(result.body, undefined);
});
test('manual actions validate coordinates, keys and bounded text', async () => {
  const { adapter, calls } = await fixture();
  await adapter.input({ action: 'click', x: 100, y: 200 }); assert.deepEqual(calls.click, [100, 200]);
  for (const input of [{ action: 'click', x: 1280, y: 0 }, { action: 'click', x: NaN, y: 0 },
    { action: 'key', key: 'F12' }, { action: 'text', text: 'a'.repeat(4097) }, { action: 'eval', text: 'any' }])
    await assert.rejects(adapter.input(input));
});
test('verification navigation permits only bounded exact-origin URLs', async () => {
  const { adapter, calls } = await fixture();
  await adapter.navigate({ url: 'https://example.com/verification' });
  assert.equal(calls.navigation, 'https://example.com/verification');
  for (const url of ['https://other.example/', 'http://example.com/', 'https://user:pass@example.com/', 'https://example.com/#script', 'https://example.com/' + 'x'.repeat(8192)])
    await assert.rejects(adapter.navigate({ url }), /origin_denied/);
});
test('same-context fetch validates origin and strips all except bounded content-type', async () => {
  const { adapter, calls } = await fixture();
  const output = await adapter.request({ method: 'GET', url: 'https://example.com/private' });
  assert.deepEqual(output, { status: 200, headers: { 'content-type': 'application/json' }, bodyBase64: 'e30=', cookies: [] });
  assert.match(calls.script, /credentials: 'same-origin'/);
  assert.match(calls.script, /redirect: 'error'/);
  await assert.rejects(adapter.request({ method: 'GET', url: 'https://other.example/' }), /origin_denied/);
  await assert.rejects(adapter.request({ method: 'DELETE', url: 'https://example.com/' }));
  calls.result = { error: 'secret-untrusted-error' };
  await assert.rejects(adapter.request({ method: 'GET', url: 'https://example.com/' }), error => error.message === 'browser_request_failed');
});
test('CONNECT proxy refuses other authority before opening an upstream socket', async () => {
  const proxy = await pinnedProxy('https://example.com', '8.8.8.8');
  try {
    await new Promise((resolve, reject) => {
      const request = http.request(proxy.url, { method: 'CONNECT', path: '127.0.0.1:443' });
      request.on('error', () => resolve());
      request.on('connect', () => reject(new Error('unexpected tunnel')));
      request.setTimeout(1000, () => request.destroy()); request.end();
    });
    const response = await fetch(proxy.url); assert.equal(response.status, 403);
  } finally { await proxy.close(); }
});
