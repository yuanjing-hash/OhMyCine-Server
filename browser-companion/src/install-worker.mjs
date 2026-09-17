// Spawned only after explicit acceptance. Never invoked by package installation.
try {
  const { ensureBinary } = await import('cloakbrowser');
  const version = process.platform === 'linux' && process.arch === 'arm64'
    ? '146.0.7680.177.3' : '146.0.7680.177.5';
  const executablePath = await ensureBinary(undefined, version);
  process.send?.({ executablePath });
  process.disconnect?.();
} catch {
  process.send?.({ error: 'install_failed' });
  process.exitCode = 1;
  process.disconnect?.();
}
