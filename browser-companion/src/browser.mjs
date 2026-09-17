import { Fault, requireThat, sameOrigin, resolvePublic, resourceURL } from './policy.mjs';
import { pinnedProxy } from './proxy.mjs';
import { validateCookies, exportCookies } from './cookies.mjs';

export const WIDTH = 1280, HEIGHT = 800, MAX_BODY = 2 * 1024 * 1024;

function samePageOrigin(value, origin) {
  try { const url = new URL(value); url.hash = ''; return sameOrigin(url.href, origin); }
  catch { return false; }
}

export async function openBrowser(executablePath, origin, dependencies = {}) {
  const cookies = validateCookies(dependencies.cookies ?? [], origin);
  const address = await (dependencies.resolvePublic || resolvePublic)(origin, undefined, dependencies.allowTUNFakeIP === true);
  let networkErrorCode = '', blockedResourceCount = 0, mainDocumentMethod = 'GET';
  const blocked = code => {
    blockedResourceCount = Math.min(blockedResourceCount + 1, 10000);
    networkErrorCode = code === 'tun_fake_ip_requires_opt_in' || code === 'resource_limit_exceeded' ? code
      : code === 'network_timeout' ? 'resource_network_timeout'
      : code === 'resource_network_failed' ? code : 'resource_network_denied';
  };
  const proxy = await (dependencies.pinnedProxy || pinnedProxy)(origin, address, {
    allowTUNFakeIP: dependencies.allowTUNFakeIP === true, onBlocked: blocked,
  });
  let browser;
  try {
    const chromium = dependencies.chromium || (await import('playwright-core')).chromium;
    const browserEnv = {};
    for (const key of ['PATH', 'Path', 'SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'HOME', 'USERPROFILE',
      'LOCALAPPDATA', 'APPDATA', 'LANG', 'LC_ALL', 'DISPLAY', 'XDG_RUNTIME_DIR'])
      if (process.env[key]) browserEnv[key] = process.env[key];
    // Playwright controls the officially acquired Cloak executable. Never use
    // upstream launch defaults that add --no-sandbox, auto-update or license IO.
    browser = await chromium.launch({ executablePath, headless: true, chromiumSandbox: true, env: browserEnv,
      timeout: 30_000, proxy: { server: proxy.url, bypass: '<-loopback>' },
      args: ['--disable-quic', '--disable-background-networking', '--disable-component-update',
        '--disable-domain-reliability', '--disable-features=DnsOverHttps,AsyncDns',
        '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1',
        '--force-webrtc-ip-handling-policy=disable_non_proxied_udp'],
    }).catch(() => { throw new Fault('browser_launch_failed', 503); });
    const context = await browser.newContext({ viewport: { width: WIDTH, height: HEIGHT },
      acceptDownloads: false, serviceWorkers: 'block', permissions: [], ignoreHTTPSErrors: false });
    context.setDefaultTimeout(10_000);
    context.setDefaultNavigationTimeout(20_000);
    await context.route('**/*', route => {
      const request = route.request();
      try {
        resourceURL(request.url());
        if (request.isNavigationRequest?.() && !request.frame().parentFrame()) {
          requireThat(sameOrigin(request.url(), origin), 'navigation_denied');
          if (request.method() !== 'GET') mainDocumentMethod = request.method();
        }
        return route.continue();
      } catch { blocked('network_denied'); return route.abort('blockedbyclient'); }
    });
    await context.routeWebSocket('**/*', socket => socket.close());
    if (cookies.length) await context.addCookies(cookies);
    const page = await context.newPage();
    page.on('response', response => {
      const request = response.request();
      if (request.isNavigationRequest() && !request.frame().parentFrame() && sameOrigin(response.url(), origin))
        mainDocumentMethod = request.method();
    });
    context.on('page', p => { if (p !== page) p.close().catch(() => {}); });
    page.on('dialog', d => d.dismiss().catch(() => {}));
    page.on('download', d => d.cancel().catch(() => {}));
    await page.goto(origin, { waitUntil: 'domcontentloaded' })
      .catch(() => { throw new Fault('browser_navigation_failed', 502); });
    return {
      async cookies() { return await exportCookies(context, origin); },
      async reload() {
        requireThat(samePageOrigin(page.url(), origin), 'navigation_denied');
        // Do not silently replay a form POST while refreshing verification.
        requireThat(mainDocumentMethod === 'GET', 'browser_reload_post_denied');
        networkErrorCode = ''; blockedResourceCount = 0;
        proxy.resetFailures?.();
        await page.reload({ waitUntil: 'domcontentloaded' })
          .catch(() => { throw new Fault('browser_navigation_failed', 502); });
        requireThat(samePageOrigin(page.url(), origin), 'navigation_denied');
        return {};
      },
      async navigate(input) {
        requireThat(typeof input.url === 'string' && input.url.length <= 8192 && sameOrigin(input.url, origin), 'origin_denied');
        await page.goto(input.url, { waitUntil: 'domcontentloaded' })
          .catch(() => { throw new Fault('browser_navigation_failed', 502); });
        requireThat(samePageOrigin(page.url(), origin), 'navigation_denied');
        return {};
      },
      async snapshot() {
        requireThat(samePageOrigin(page.url(), origin), 'navigation_denied');
        const image = await page.screenshot({ type: 'png', fullPage: false, timeout: 10_000 });
        requireThat(image.length <= 2 * 1024 * 1024, 'response_too_large');
        return { imageBase64: image.toString('base64'), mimeType: 'image/png', width: WIDTH, height: HEIGHT,
          networkErrorCode, blockedResourceCount };
      },
      async input(input) {
        requireThat(samePageOrigin(page.url(), origin), 'navigation_denied');
        if (input.action === 'click') {
          requireThat(Number.isFinite(input.x) && input.x >= 0 && input.x < WIDTH &&
            Number.isFinite(input.y) && input.y >= 0 && input.y < HEIGHT);
          await page.mouse.click(input.x, input.y);
        } else if (input.action === 'text') {
          requireThat(typeof input.text === 'string' && input.text.length <= 4096);
          await page.keyboard.insertText(input.text);
        } else if (input.action === 'key') {
          requireThat(['Tab', 'Enter', 'Backspace', 'Delete', 'Escape', 'ArrowLeft', 'ArrowRight',
            'ArrowUp', 'ArrowDown', 'Home', 'End', 'ControlOrMeta+A'].includes(input.key));
          await page.keyboard.press(input.key);
        } else if (input.action === 'scroll') {
          requireThat(Number.isFinite(input.deltaY) && Math.abs(input.deltaY) <= 2000);
          await page.mouse.wheel(0, input.deltaY);
        } else throw new Fault('invalid_action');
        return {};
      },
      async request(input) {
        requireThat(sameOrigin(input.url, origin) && samePageOrigin(page.url(), origin), 'origin_denied');
        requireThat(['GET', 'POST'].includes(input.method));
        requireThat(input.body === undefined || (typeof input.body === 'string' && Buffer.byteLength(input.body) <= 65536));
        requireThat(input.method !== 'GET' || !input.body);
        const contentType = input.contentType || 'application/x-www-form-urlencoded';
        requireThat(['application/x-www-form-urlencoded', 'application/json', 'text/plain'].includes(contentType));
        // Fixed host-owned script, not caller-supplied JS. Fetch uses the same
        // actual browser context (not APIRequestContext's separate HTTP stack).
        const pending = page.evaluate(async ({ url, method, body, contentType, max }) => {
          const controller = new AbortController();
          const timer = setTimeout(() => controller.abort(), 15_000);
          try {
            const response = await fetch(url, { method, body: method === 'POST' ? body : undefined,
              headers: { 'Content-Type': contentType }, credentials: 'same-origin',
              redirect: 'error', signal: controller.signal });
            const reader = response.body?.getReader();
            const chunks = []; let size = 0;
            if (reader) for (;;) {
              const { done, value } = await reader.read();
              if (done) break;
              size += value.length;
              if (size > max) { await reader.cancel(); return { error: 'response_too_large' }; }
              chunks.push(value);
            }
            const bytes = new Uint8Array(size); let offset = 0;
            for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length; }
            let binary = '';
            for (let start = 0; start < bytes.length; start += 8192)
              binary += String.fromCharCode(...bytes.subarray(start, start + 8192));
            return { status: response.status, headers: { 'content-type': response.headers.get('content-type') || '' },
              bodyBase64: btoa(binary) };
          } catch { return { error: 'browser_request_failed' }; }
          finally { clearTimeout(timer); }
        }, { url: input.url, method: input.method, body: input.body, contentType, max: MAX_BODY });
        // Page JS is untrusted and can replace fetch or timers. A host-side
        // deadline remains authoritative even if the in-page timer is disabled.
        let timer;
        const result = await Promise.race([pending, new Promise((_, reject) => {
          timer = setTimeout(() => {
            browser.close().catch(() => {}); proxy.close().catch(() => {});
            reject(new Fault('browser_request_timeout', 504));
          }, 20_000);
        })]).finally(() => clearTimeout(timer));
        if (result?.error) throw new Fault(result.error === 'response_too_large' ? 'response_too_large' : 'browser_request_failed', 502);
        requireThat(Number.isInteger(result?.status) && result.status >= 100 && result.status <= 599 &&
          typeof result.bodyBase64 === 'string' && result.bodyBase64.length <= Math.ceil(MAX_BODY / 3) * 4 &&
          result.bodyBase64.length % 4 === 0 && /^[A-Za-z0-9+/]*={0,2}$/.test(result.bodyBase64), 'invalid_browser_response');
        const bytes = Buffer.from(result.bodyBase64, 'base64');
        requireThat(bytes.length <= MAX_BODY && bytes.toString('base64') === result.bodyBase64, 'invalid_browser_response');
        const mime = result.headers?.['content-type'];
        requireThat(typeof mime === 'string' && mime.length <= 256 && !/[\r\n]/.test(mime), 'invalid_browser_response');
        return { status: result.status, bodyBase64: result.bodyBase64, headers: { 'content-type': mime },
          cookies: await exportCookies(context, origin) };
      },
      async close() { try { await browser.close(); } finally { await proxy.close(); } },
    };
  } catch (err) {
    await browser?.close().catch(() => {});
    await proxy.close();
    throw err;
  }
}
