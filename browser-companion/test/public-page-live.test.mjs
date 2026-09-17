import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { openBrowser } from '../src/browser.mjs';

// Explicit, credential-free deployment check. Never install/acquire an engine,
// read the runtime DB/profile/Cookies, or accept a license from this test.
// Use an already installed executable; the context/profile is temporary.
test('public page loads external resources in isolated installed browser', {
  skip: process.env.OMC_PUBLIC_BROWSER_PROBE !== '1', timeout: 70_000,
}, async t => {
  const executablePath = process.env.OMC_PUBLIC_BROWSER_EXECUTABLE;
  assert.ok(executablePath && path.isAbsolute(executablePath));
  const origin = 'https://www.xn--kivn76b41nnhi.com';
  const { chromium } = await import('playwright-core');
  let realBrowser, page, adapter;
  const resources = new Map();
  const failures = new Map();
  let workers = 0;
  const record = (map, key) => map.set(key, (map.get(key) || 0) + 1);
  const wrapper = { launch: async options => {
    realBrowser = await chromium.launch(options);
    const newContext = realBrowser.newContext.bind(realBrowser);
    realBrowser.newContext = async options => {
      const context = await newContext(options);
      context.on('response', response => {
        let external;
        try { external = new URL(response.url()).origin !== origin; } catch { return; }
        if (external) record(resources, `${response.request().resourceType()}:${response.status()}`);
      });
      context.on('requestfailed', request => {
        // Aggregate only: no URL, headers, body, Cookie or raw browser errors.
        record(failures, request.resourceType());
      });
      context.on('page', p => { page = p; p.on('worker', () => { workers++; }); });
      return context;
    };
    return realBrowser;
  } };
  const deadline = setTimeout(() => { realBrowser?.close().catch(() => {}); }, 60_000);
  try {
    adapter = await openBrowser(executablePath, origin, {
      chromium: wrapper, cookies: [], allowTUNFakeIP: process.env.OMC_PUBLIC_BROWSER_TUN === '1',
    });
    assert.equal(new URL(page.url()).origin, origin);
    await page.waitForTimeout(8_000);
    const before = Object.fromEntries(resources);
    const image = await adapter.snapshot();
    const directory = await mkdtemp(path.join(tmpdir(), 'omc-public-browser-check-'));
    const screenshotPath = path.join(directory, 'public-page.png');
    await writeFile(screenshotPath, Buffer.from(image.imageBase64, 'base64'));
    t.diagnostic(JSON.stringify({ externalResources: before, failedResourceTypes: Object.fromEntries(failures), workers, screenshotPath }));
    assert.ok([...resources].some(([key, count]) => key.startsWith('script:2') && count > 0), 'no successful external script');
    assert.ok([...resources].some(([key, count]) => key.startsWith('stylesheet:2') && count > 0), 'no successful external stylesheet');
    // Reload does not create another session or invoke a Host login operation.
    await adapter.reload();
    assert.equal(new URL(page.url()).origin, origin);
    t.diagnostic('true page reload completed; account login acceptance was not tested');
  } finally {
    clearTimeout(deadline);
    if (adapter) await adapter.close();
    else await realBrowser?.close();
  }
});
