import http from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { Fault, requireThat } from './policy.mjs';

export function createServer(runtime, token) {
  requireThat(typeof token === 'string' && /^[A-Za-z0-9_-]{32,256}$/.test(token), 'token_required');
  const expected = Buffer.from(`Bearer ${token}`);
  const server = http.createServer({ requestTimeout: 20_000, headersTimeout: 10_000, maxHeaderSize: 8192 }, async (req, res) => {
    res.setHeader('Cache-Control', 'no-store');
    res.setHeader('Content-Type', 'application/json');
    res.setHeader('X-Content-Type-Options', 'nosniff');
    try {
      const auth = Buffer.from(req.headers.authorization || '');
      requireThat(auth.length === expected.length && timingSafeEqual(auth, expected), 'unauthorized', 401);
      requireThat(!req.headers.origin && !req.headers['sec-fetch-site'], 'browser_client_denied', 403);
      requireThat(req.method === 'POST' && req.headers['content-type']?.split(';')[0] === 'application/json');
      requireThat(/^\/v1\/(status|install|shutdown|session\/(create|snapshot|reload|input|navigate|request|cookies|close))$/.test(req.url), 'not_found', 404);
      const chunks = []; let length = 0;
      for await (const chunk of req) {
        length += chunk.length;
        requireThat(length <= 128 * 1024, 'request_too_large', 413);
        chunks.push(chunk);
      }
      let input;
      try { input = JSON.parse(Buffer.concat(chunks).toString('utf8')); } catch { throw new Fault('invalid_json'); }
      if (req.url === '/v1/shutdown') {
        await runtime.close();
        res.once('finish', () => { server.close(); server.closeAllConnections(); });
        res.end('{}');
        return;
      }
      const controller = new AbortController();
      const disconnected = () => { if (!res.writableFinished) controller.abort(); };
      res.once('close', disconnected);
      if (res.destroyed) controller.abort();
      let output;
      try { output = await runtime.dispatch(req.url, input, { signal: controller.signal }); }
      finally { res.removeListener('close', disconnected); }
      res.end(JSON.stringify(output));
    } catch (error) {
      // Never return upstream exception messages, selectors, URLs or secrets.
      res.statusCode = error instanceof Fault ? error.status : 503;
      res.end(JSON.stringify({ error: error instanceof Fault ? error.code : 'browser_operation_failed' }));
    }
  });
  return server;
}
