import http from 'node:http';
import net from 'node:net';
import { Fault, requireThat, resourceURL, resolvePublic, publicIP, tunFakeIP } from './policy.mjs';

// Context-private HTTPS egress. Every destination is resolved once, checked in
// full and pinned numerically, including Worker requests outside page routing.
export async function pinnedProxy(origin, address, options = {}) {
  const { allowTUNFakeIP = false, onBlocked = () => {}, resolver, connect = net.connect } = options;
  requireThat(publicIP(address) || (allowTUNFakeIP === true && tunFakeIP(address)), 'network_denied');
  const pins = new Map([[resourceURL(origin).hostname, Promise.resolve(address)]]);
  const failedPins = new Set();
  const sockets = new Set();
  const dnsQueue = [];
  let resolving = 0, closed = false;
  const deny = error => onBlocked(error instanceof Fault ? error.code : 'resource_network_failed');
  function acquireDNS() {
    if (closed) return Promise.reject(new Fault('network_denied'));
    if (resolving < 8) { resolving++; return Promise.resolve(); }
    return new Promise((resolve, reject) => {
      const waiter = { resolve, reject, timer: null };
      waiter.timer = setTimeout(() => {
        const index = dnsQueue.indexOf(waiter);
        if (index >= 0) dnsQueue.splice(index, 1);
        reject(new Fault('network_timeout'));
      }, 10_000);
      dnsQueue.push(waiter);
    });
  }
  async function resolveHost(host) {
    await acquireDNS();
    try { return await resolvePublic(`https://${host}`, resolver, allowTUNFakeIP); }
    finally {
      resolving--;
      const waiter = dnsQueue.shift();
      if (waiter) { clearTimeout(waiter.timer); resolving++; waiter.resolve(); }
    }
  }
  async function pin(host) {
    if (pins.has(host)) return pins.get(host);
    requireThat(pins.size < 128 && !closed, 'resource_limit_exceeded');
    const pending = resolveHost(host).catch(error => { failedPins.add(host); throw error; });
    // Keep failed resolutions cached too: page retries cannot create a DNS storm.
    pins.set(host, pending);
    return pending;
  }
  const server = http.createServer({ requestTimeout: 10_000, headersTimeout: 10_000, maxHeaderSize: 8192 }, (_req, res) => {
    deny(new Fault('network_denied')); res.writeHead(403); res.end();
  });
  server.on('connection', s => {
    if (closed || sockets.size >= 128) { deny(new Fault('resource_limit_exceeded')); s.destroy(); return; }
    sockets.add(s); s.setTimeout(30_000, () => s.destroy());
    s.on('error', () => {}); s.on('close', () => sockets.delete(s));
  });
  server.on('connect', async (req, client, head) => {
    try {
      requireThat(typeof req.url === 'string' && req.url.endsWith(':443'), 'network_denied');
      const url = resourceURL(`https://${req.url}`);
      requireThat(req.url.toLowerCase() === `${url.hostname}:443` && !req.headers.upgrade, 'network_denied');
      const numeric = await pin(url.hostname);
      if (closed || client.destroyed) return;
      requireThat(sockets.size < 128, 'resource_limit_exceeded');
      const upstream = connect({ host: numeric, port: 443 });
      sockets.add(upstream);
      let received = false;
      upstream.once('data', () => { received = true; });
      upstream.setTimeout(30_000, () => {
        // No upstream response is a failure; an established idle TLS tunnel
        // may just be normal browser keepalive (encrypted bodies are opaque).
        if (!received) deny(new Fault('network_timeout'));
        upstream.destroy();
      });
      upstream.on('close', () => { sockets.delete(upstream); client.destroy(); });
      upstream.on('error', () => { deny(new Fault('resource_network_failed')); client.destroy(); });
      client.on('error', () => upstream.destroy());
      client.on('close', () => upstream.destroy());
      upstream.once('connect', () => {
        if (closed || client.destroyed) { upstream.destroy(); return; }
        client.write('HTTP/1.1 200 Connection Established\r\n\r\n');
        if (head.length) upstream.write(head);
        client.pipe(upstream); upstream.pipe(client);
      });
    } catch (error) { deny(error); client.destroy(); }
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    resetFailures() {
      // Only deliberate reload retries settled failures. Successful pins and
      // in-flight DNS promises remain immutable across reload/rebinding.
      for (const host of failedPins) pins.delete(host);
      failedPins.clear();
    },
    close: async () => {
      if (closed) return;
      closed = true;
      for (const waiter of dnsQueue.splice(0)) { clearTimeout(waiter.timer); waiter.reject(new Fault('network_denied')); }
      for (const s of sockets) s.destroy();
      await new Promise(resolve => server.close(resolve));
    },
  };
}
