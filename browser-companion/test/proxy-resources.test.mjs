import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { Duplex } from 'node:stream';
import { pinnedProxy } from '../src/proxy.mjs';

function tunnel(proxy, authority) {
  return new Promise(resolve => {
    const req = http.request(proxy.url, { method: 'CONNECT', path: authority });
    req.on('error', () => resolve(false));
    req.on('connect', (_res, socket) => { socket.destroy(); resolve(true); });
    req.setTimeout(1000, () => req.destroy()); req.end();
  });
}
function connector(calls) {
  return options => {
    calls.push(options);
    const socket = new Duplex({ read() {}, write(_chunk, _enc, cb) { cb(); } });
    socket.setTimeout = () => socket;
    queueMicrotask(() => socket.emit('connect'));
    return socket;
  };
}
test('each public hostname pins once; DNS changes never retarget an existing context', async () => {
  const calls = [], lookups = [];
  const proxy = await pinnedProxy('https://example.com', '8.8.8.8', {
    connect: connector(calls), resolver: async host => {
      lookups.push(host); return [{ address: lookups.length > 2 ? '127.0.0.1' : '1.1.1.1' }];
    },
  });
  try {
    for (const host of ['example.com', 'cdn.example', 'worker.example', 'cdn.example'])
      assert.equal(await tunnel(proxy, `${host}:443`), true);
    assert.deepEqual(lookups, ['cdn.example', 'worker.example']);
    assert.deepEqual(calls.map(v => v.host), ['8.8.8.8', '1.1.1.1', '1.1.1.1', '1.1.1.1']);
    assert.equal(calls.every(v => v.port === 443), true);
  } finally { await proxy.close(); }
});
test('proxy rejects literals, unsafe ports/schemes, mixed private answers and caches denials', async () => {
  const calls = [], denied = []; let lookups = 0;
  const proxy = await pinnedProxy('https://example.com', '8.8.8.8', {
    connect: connector(calls), onBlocked: code => denied.push(code),
    resolver: async () => { lookups++; return [{ address: '1.1.1.1' }, { address: '169.254.169.254' }]; },
  });
  try {
    for (const authority of ['127.0.0.1:443', '[::1]:443', '0x7f000001:443', 'cdn.example:80',
      'cdn.example:444', 'user:pass@cdn.example:443', 'cdn.example:443/path', 'bad.example:443', 'bad.example:443'])
      assert.equal(await tunnel(proxy, authority), false, authority);
    assert.equal(lookups, 1); assert.equal(calls.length, 0); assert.equal(denied.length, 9);
  } finally { await proxy.close(); }
});
test('third-party Fake-IP requires deployment opt-in and mixed private DNS still fails', async () => {
  for (const enabled of [false, true]) {
    const calls = [];
    const proxy = await pinnedProxy('https://example.com', '8.8.8.8', {
      connect: connector(calls), allowTUNFakeIP: enabled,
      resolver: async host => host === 'mixed.example' ? [{ address: '198.18.7.1' }, { address: '10.0.0.1' }] : [{ address: '198.18.7.1' }],
    });
    try {
      assert.equal(await tunnel(proxy, 'cdn.example:443'), enabled);
      assert.equal(await tunnel(proxy, 'mixed.example:443'), false);
      assert.equal(await tunnel(proxy, '198.18.7.1:443'), false);
      if (enabled) assert.deepEqual(calls, [{ host: '198.18.7.1', port: 443 }]);
    } finally { await proxy.close(); }
  }
});

test('explicit reload retries failed DNS but preserves successful pins', async () => {
  let lookups = 0;
  const calls = [];
  const proxy = await pinnedProxy('https://example.com', '8.8.8.8', {
    connect: connector(calls), resolver: async () => {
      lookups++;
      if (lookups === 1) throw new Error('synthetic DNS failure');
      return [{ address: lookups === 2 ? '1.1.1.1' : '127.0.0.1' }];
    },
  });
  try {
    assert.equal(await tunnel(proxy, 'cdn.example:443'), false);
    assert.equal(await tunnel(proxy, 'cdn.example:443'), false);
    assert.equal(lookups, 1);
    proxy.resetFailures();
    assert.equal(await tunnel(proxy, 'cdn.example:443'), true);
    proxy.resetFailures();
    assert.equal(await tunnel(proxy, 'cdn.example:443'), true);
    assert.equal(lookups, 2);
    assert.equal(calls.every(value => value.host === '1.1.1.1'), true);
  } finally { await proxy.close(); }
});

test('DNS concurrency is queued and bounded even for Worker traffic bypassing page routing', async () => {
  const waiting = [], denied = [];
  const proxy = await pinnedProxy('https://example.com', '8.8.8.8', {
    connect: connector([]), onBlocked: code => denied.push(code),
    resolver: async () => await new Promise(resolve => waiting.push(resolve)),
  });
  try {
    const requests = Array.from({ length: 9 }, (_, index) => tunnel(proxy, `worker${index}.example:443`));
    for (let n = 0; n < 100 && waiting.length !== 8; n++) await new Promise(resolve => setTimeout(resolve, 5));
    assert.equal(waiting.length, 8);
    proxy.resetFailures(); // In-flight work cannot be evicted to bypass the limit.
    assert.equal(waiting.length, 8);
    for (const resolve of waiting.slice()) resolve([{ address: '1.1.1.1' }]);
    for (let n = 0; n < 100 && waiting.length !== 9; n++) await new Promise(resolve => setTimeout(resolve, 5));
    assert.equal(waiting.length, 9);
    waiting[8]([{ address: '1.1.1.1' }]);
    assert.deepEqual(await Promise.all(requests), Array(9).fill(true));
    assert.deepEqual(denied, []);
  } finally {
    for (const resolve of waiting) resolve([{ address: '1.1.1.1' }]);
    await proxy.close();
  }
});
