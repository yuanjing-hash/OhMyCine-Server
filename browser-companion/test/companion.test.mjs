import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { Fault, numericIP, originURL, sameOrigin, resolvePinnedAddress } from '../src/policy.mjs';
import { Runtime, SESSION_TTL, supportedPlatform } from '../src/runtime.mjs';
import { createServer } from '../src/server.mjs';

test('canonical HTTPS origin and numeric DNS address pinning', async () => {
  assert.equal(originURL('https://星际穿越.com'), 'https://xn--kivn76b41nnhi.com');
  for (const value of ['http://example.com', 'https://u:p@example.com', 'https://example.com:444',
    'https://example.com/path', 'https://example.com?x=y', 'https://example.com.']) assert.throws(() => originURL(value));
  for (const ip of ['127.0.0.1', '10.1.1.1', '169.254.169.254', '198.18.7.137',
    '8.8.8.8', '::1', '::ffff:127.0.0.1', 'fdfe:dcba:9876::89']) assert.equal(numericIP(ip), true, ip);
  for (const value of ['', 'not-an-address', '127.0.0.1.evil.test', 'fe80::1%eth0'])
    assert.equal(numericIP(value), false, value);
  assert.equal(sameOrigin('https://other.example/', 'https://example.com'), false);
  assert.equal(sameOrigin('https://u:p@example.com/', 'https://example.com'), false);
  assert.equal(await resolvePinnedAddress('https://example.com', async () => [
    { address: '198.18.7.137' }, { address: 'fdfe:dcba:9876::89' }]), '198.18.7.137');
  assert.equal(await resolvePinnedAddress('https://example.com', async () => [
    { address: 'fdfe:dcba:9876::89' }, { address: '10.0.0.1' }]), 'fdfe:dcba:9876::89');
  await assert.rejects(resolvePinnedAddress('https://example.com', async () => [{ address: 'bad-ip' }]), /network_denied/);
  await assert.rejects(resolvePinnedAddress('https://example.com', async () => []), /network_denied/);
});

test('address-range allowance does not allow direct IP origins or malformed DNS answers', async () => {
  let lookups = 0;
  assert.equal(await resolvePinnedAddress('https://example.com', async () => { lookups++; return [{ address: '127.0.0.1' }]; }), '127.0.0.1');
  assert.equal(lookups, 1);
  for (const value of ['https://8.8.8.8', 'https://[::1]', 'https://0xc6120789'])
    assert.throws(() => originURL(value), /invalid_origin/);
  await assert.rejects(resolvePinnedAddress('https://example.com', async () => [{ address: '10.0.0.1' }, { address: 'evil.test' }]), /network_denied/);
});

test('network failure remains distinct from a credential failure', async t => {
  const runtime = await fixture(t, { opener: async () => { throw new Fault('network_denied'); } });
  runtime.state = 'ready'; runtime.executablePath = '/synthetic';
  await assert.rejects(runtime.dispatch('/v1/session/create', { identity: 'owner', origin: 'https://example.com' }), /network_denied/);
  assert.equal(runtime.status().installed, true);
  assert.equal(runtime.status().runtimeError, 'network_denied');
  assert.equal('tunFakeIPEnabled' in runtime.status(), false);
});

async function fixture(t, extras = {}) {
  const dir = await mkdtemp(path.join(tmpdir(), 'omc-companion-test-'));
  const runtime = new Runtime({ stateDir: dir, ...extras });
  t.after(async () => { await runtime.close(); await rm(dir, { recursive: true, force: true }); });
  await runtime.initialize();
  return runtime;
}

test('reload preserves the owned session and refuses another identity', async t => {
  let reloads = 0;
  const runtime = await fixture(t, { opener: async () => ({ reload: async () => { reloads++; return {}; }, close: async () => {} }) });
  runtime.state = 'ready';
  const created = await runtime.dispatch('/v1/session/create', { identity: 'owner', origin: 'https://example.com' });
  await assert.rejects(runtime.dispatch('/v1/session/reload', { sessionId: created.sessionId, identity: 'other' }), /session_unavailable/);
  assert.deepEqual(await runtime.dispatch('/v1/session/reload', { sessionId: created.sessionId, identity: 'owner' }), {});
  assert.equal(reloads, 1);
  assert.equal(runtime.session.id, created.sessionId);
});
test('license refusal never starts installer; explicit install persists only private metadata', async t => {
  let installs = 0;
  const runtime = await fixture(t);
  runtime.installer = async () => {
    installs++;
    const dir = path.join(runtime.stateDir, 'cloak'); await mkdir(dir);
    const file = path.join(dir, 'synthetic-browser'); await writeFile(file, 'fixture'); return file;
  };
  assert.throws(() => runtime.install({ licenseAccepted: false }), /license_acceptance_required/);
  assert.equal(installs, 0);
  assert.equal(runtime.install({ licenseAccepted: true }).state, 'installing');
  await runtime.installPromise;
  assert.equal(installs, 1); assert.equal(runtime.status().state, 'ready');
  const restarted = new Runtime({ stateDir: runtime.stateDir }); await restarted.initialize();
  assert.equal(restarted.status().state, 'ready');
  assert.equal(supportedPlatform('win32', 'arm64'), false);
  assert.equal(supportedPlatform('linux', 'arm64'), true);
});
test('installation cannot adopt executable outside private cache', async t => {
  const runtime = await fixture(t);
  await mkdir(path.join(runtime.stateDir, 'cloak'));
  const other = path.join(runtime.stateDir, 'not-browser'); await writeFile(other, 'fixture');
  runtime.installer = async () => other;
  runtime.install({ licenseAccepted: true }); await runtime.installPromise;
  assert.equal(runtime.status().state, 'install_failed');
});
test('one context, ownership binding, expiry and close', async t => {
  let now = 1000, closed = 0, opened = 0;
  const runtime = await fixture(t, { clock: () => now, opener: async () => {
    opened++; return { close: async () => { closed++; }, snapshot: async () => ({ imageBase64: 'fixture' }) };
  } });
  await assert.rejects(runtime.dispatch('/v1/session/create', { identity: 'owner', origin: 'https://example.com' }), /browser_not_ready/);
  runtime.state = 'ready';
  const session = await runtime.dispatch('/v1/session/create', { identity: 'owner', origin: 'https://example.com' });
  await assert.rejects(runtime.dispatch('/v1/session/create', { identity: 'other', origin: 'https://example.com' }), /browser_busy/);
  await assert.rejects(runtime.dispatch('/v1/session/snapshot', { sessionId: session.sessionId, identity: 'other' }), /session_unavailable/);
  assert.deepEqual(await runtime.dispatch('/v1/session/snapshot', { sessionId: session.sessionId, identity: 'owner' }), { imageBase64: 'fixture' });
  now += SESSION_TTL;
  await assert.rejects(runtime.dispatch('/v1/session/snapshot', { sessionId: session.sessionId, identity: 'owner' }), /session_unavailable/);
  assert.equal(closed, 1); assert.equal(opened, 1);
});
test('concurrent context creation cannot exceed one browser', async t => {
  let resolveOpen;
  const runtime = await fixture(t, { opener: () => new Promise(resolve => { resolveOpen = resolve; }) });
  runtime.state = 'ready';
  const pending = runtime.dispatch('/v1/session/create', { identity: 'a', origin: 'https://example.com' });
  await Promise.resolve();
  await assert.rejects(runtime.dispatch('/v1/session/create', { identity: 'b', origin: 'https://example.com' }), /browser_busy/);
  resolveOpen({ close: async () => {} }); await pending;
});
test('HTTP authentication, browser-origin denial and safe exception envelope', async t => {
  const token = 'a'.repeat(64);
  const server = createServer({ dispatch: async () => { throw new Error('secret-cookie=never-return'); } }, token);
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); server.close(); });
  const url = `http://127.0.0.1:${server.address().port}/v1/status`;
  const call = headers => fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json', ...headers }, body: '{}' });
  assert.equal((await call({})).status, 401);
  assert.equal((await call({ Authorization: `Bearer ${token}`, Origin: 'https://evil.example' })).status, 403);
  const response = await call({ Authorization: `Bearer ${token}` });
  assert.equal(response.status, 503);
  assert.deepEqual(await response.json(), { error: 'browser_operation_failed' });
  assert.equal(response.headers.get('cache-control'), 'no-store');
});

test('authenticated shutdown closes runtime and listener', async t => {
  let closed = false;
  const token = 'b'.repeat(64);
  const server = createServer({ close: async () => { closed = true; } }, token);
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); server.close(); });
  const result = await fetch(`http://127.0.0.1:${server.address().port}/v1/shutdown`, {
    method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: '{}',
  });
  assert.equal(result.status, 200); assert.deepEqual(await result.json(), {});
  assert.equal(closed, true); assert.equal(server.listening, false);
});

test('shutdown during context launch closes the newly acquired browser', async t => {
  let resolveOpen, closed = false;
  const runtime = await fixture(t, { opener: () => new Promise(resolve => { resolveOpen = resolve; }) });
  runtime.state = 'ready';
  const pending = runtime.dispatch('/v1/session/create', { identity: 'a', origin: 'https://example.com' });
  await Promise.resolve(); await runtime.close();
  resolveOpen({ close: async () => { closed = true; } });
  await assert.rejects(pending, /browser_stopping/); assert.equal(closed, true);
});

test('cancelled create discards late browser and permits a subsequent login', async t => {
  let resolveOpen, closed = 0;
  const runtime = await fixture(t, { opener: () => new Promise(resolve => { resolveOpen = resolve; }) });
  runtime.state = 'ready';
  const controller = new AbortController();
  const pending = runtime.dispatch('/v1/session/create', { identity: 'a', origin: 'https://example.com' }, { signal: controller.signal });
  await Promise.resolve(); controller.abort();
  resolveOpen({ close: async () => { closed++; } });
  await assert.rejects(pending, /request_cancelled/);
  assert.equal(closed, 1); assert.equal(runtime.session, null); assert.equal(runtime.busy, false);
  runtime.opener = async () => ({ close: async () => {} });
  assert.ok((await runtime.dispatch('/v1/session/create', { identity: 'b', origin: 'https://example.com' })).sessionId);
});

test('HTTP client disconnection cancels in-progress browser creation', async t => {
  const token = 'c'.repeat(64);
  let started, finish;
  const active = new Promise(resolve => { started = resolve; });
  const cancelled = new Promise(resolve => { finish = resolve; });
  const server = createServer({ dispatch: async (_, __, { signal }) => {
    started();
    await new Promise(resolve => signal.addEventListener('abort', resolve, { once: true }));
    finish(); return {};
  } }, token);
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); server.close(); });
  const controller = new AbortController();
  const response = fetch(`http://127.0.0.1:${server.address().port}/v1/session/create`, {
    method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: '{}', signal: controller.signal,
  });
  await active; controller.abort();
  await assert.rejects(response);
  await cancelled;
});

test('cookies require session identity and survive replacement only through explicit host import', async t => {
  const cookie = { name: 'session', value: 'synthetic', domain: 'example.com', path: '/',
    expires: -1, httpOnly: true, secure: true, sameSite: 'Lax' };
  let restored;
  const runtime = await fixture(t, { opener: async (_, __, options) => {
    restored = options.cookies;
    return { close: async () => {}, cookies: async () => restored };
  } });
  runtime.state = 'ready';
  const session = await runtime.dispatch('/v1/session/create', { identity: 'owner', origin: 'https://example.com', cookies: [cookie] });
  assert.deepEqual(restored, [cookie]);
  await assert.rejects(runtime.dispatch('/v1/session/cookies', { identity: 'other', sessionId: session.sessionId }), /session_unavailable/);
  const binding = { identity: 'owner', sessionId: session.sessionId };
  assert.deepEqual(await runtime.dispatch('/v1/session/cookies', binding), { cookies: [cookie] });
  await runtime.dispatch('/v1/session/close', binding);
  await assert.rejects(runtime.dispatch('/v1/session/create', { identity: 'owner', origin: 'https://other.example', cookies: [cookie] }), /cookie_origin_denied/);
});
test('launch failure preserves installation but status reports failure and allows explicit retry', async t => {
  let fail = true;
  const runtime = await fixture(t, { opener: async () => {
    if (fail) throw new Error('secret=must-not-escape');
    return { close: async () => {} };
  } });
  runtime.state = 'ready'; runtime.executablePath = '/synthetic';
  const input = { identity: 'owner', origin: 'https://example.com' };
  await assert.rejects(runtime.dispatch('/v1/session/create', input), /^Error: browser_launch_failed$/);
  assert.equal(runtime.status().state, 'launch_failed');
  assert.equal(runtime.status().installed, true);
  assert.equal(runtime.status().runtimeError, 'browser_launch_failed');
  fail = false; await runtime.dispatch('/v1/session/create', input);
  assert.equal(runtime.status().state, 'ready'); assert.equal(runtime.status().runtimeError, '');
});
