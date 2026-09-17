import { Runtime } from './runtime.mjs';
import { createServer } from './server.mjs';

try {
  const runtime = new Runtime({ stateDir: process.env.OMC_CLOAK_DATA_DIR,
    allowTUNFakeIP: process.env.OMC_CLOAK_TUN_FAKE_IP === 'true' });
  const server = createServer(runtime, process.env.OMC_CLOAK_TOKEN);
  const port = Number(process.env.OMC_CLOAK_PORT || 19876);
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('invalid_port');
  await runtime.initialize();
  server.listen(port, '127.0.0.1');
  server.on('error', () => { process.stderr.write('browser_listener_failed\n'); process.exitCode = 1; });
  const expiry = setInterval(() => {
    if (!runtime.busy) runtime.expire().catch(() => {});
  }, 15_000);
  expiry.unref();
  const shutdown = async () => {
    clearInterval(expiry);
    server.close(); server.closeAllConnections();
    await runtime.close();
  };
  process.once('SIGINT', shutdown); process.once('SIGTERM', shutdown);
} catch {
  process.stderr.write('browser_configuration_failed\n');
  process.exitCode = 1;
}
