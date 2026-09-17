import { requireThat } from './policy.mjs';

// The only persisted browser state is an allowlisted cookie DTO. Never export
// storageState (localStorage may contain passwords or unrelated application data).
export function validateCookies(cookies, origin) {
  requireThat(Array.isArray(cookies) && cookies.length <= 128, 'invalid_cookies');
  requireThat(Buffer.byteLength(JSON.stringify(cookies)) <= 32768, 'invalid_cookies');
  const hostname = new URL(origin).hostname;
  const identities = new Set();
  return cookies.map(cookie => {
    requireThat(cookie && typeof cookie === 'object' && !Array.isArray(cookie), 'invalid_cookies');
    requireThat(typeof cookie.name === 'string' && /^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}$/.test(cookie.name), 'invalid_cookies');
    requireThat(typeof cookie.value === 'string' && cookie.value.length <= 4096 && /^[\x21\x23-\x2B\x2D-\x3A\x3C-\x5B\x5D-\x7E]*$/.test(cookie.value), 'invalid_cookies');
    requireThat(cookie.domain === hostname, 'cookie_origin_denied');
    requireThat(typeof cookie.path === 'string' && cookie.path.startsWith('/') && cookie.path.length <= 1024 && !/[\x00-\x20\x7f;]/.test(cookie.path), 'invalid_cookies');
    requireThat(Number.isFinite(cookie.expires) && (cookie.expires === -1 ||
      (cookie.expires >= 0 && cookie.expires <= 253402300799)), 'invalid_cookies');
    requireThat(typeof cookie.httpOnly === 'boolean' && typeof cookie.secure === 'boolean' && ['Strict', 'Lax', 'None'].includes(cookie.sameSite), 'invalid_cookies');
    const key = `${cookie.name}\n${cookie.path}`;
    requireThat(!identities.has(key), 'invalid_cookies'); identities.add(key);
    return { name: cookie.name, value: cookie.value, domain: hostname, path: cookie.path,
      expires: cookie.expires, httpOnly: cookie.httpOnly, secure: cookie.secure, sameSite: cookie.sameSite };
  });
}

export async function exportCookies(context, origin) {
  const hostname = new URL(origin).hostname;
  // Query all paths, then retain only cookies whose domain covers this exact
  // origin. Narrow domain cookies to host-only on restore: no cross-mirror reuse.
  const values = (await context.cookies()).filter(cookie => {
    const domain = cookie.domain.replace(/^\./, '');
    return domain === hostname || (cookie.domain.startsWith('.') && hostname.endsWith(`.${domain}`));
  }).map(cookie => ({ ...cookie, domain: hostname }));
  return validateCookies(values, origin);
}
