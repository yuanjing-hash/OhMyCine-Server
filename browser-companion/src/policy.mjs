import { lookup } from 'node:dns/promises';
import { isIP } from 'node:net';

export class Fault extends Error {
  constructor(code, status = 400) { super(code); this.code = code; this.status = status; }
}
export function requireThat(condition, code = 'invalid_request', status = 400) {
  if (!condition) throw new Fault(code, status);
}
export function originURL(value) {
  let u;
  try { u = new URL(value); } catch { throw new Fault('invalid_origin'); }
  requireThat(u.protocol === 'https:' && !u.username && !u.password && !u.port &&
    u.pathname === '/' && !u.search && !u.hash && !u.hostname.endsWith('.') &&
    !isIP(u.hostname.replace(/^\[|\]$/g, '')), 'invalid_origin');
  return u.origin;
}
export function sameOrigin(value, origin) {
  try {
    const u = new URL(value);
    return u.origin === origin && !u.username && !u.password && !u.hash;
  } catch { return false; }
}
export function numericIP(address) { return typeof address === 'string' && !address.includes('%') && isIP(address) !== 0; }
export function resourceURL(value) {
  let u;
  try { u = new URL(value); } catch { throw new Fault('network_denied'); }
  requireThat(value.length <= 8192 && u.protocol === 'https:' && !u.username && !u.password && !u.port &&
    !u.hostname.endsWith('.') && !isIP(u.hostname.replace(/^\[|\]$/g, '')), 'network_denied');
  return u;
}
export async function resolvePinnedAddress(origin, resolver = lookup) {
  const hostname = new URL(originURL(origin)).hostname;
  let timer;
  const results = await Promise.race([
    resolver(hostname, { all: true, verbatim: true }),
    new Promise((_, reject) => { timer = setTimeout(() => reject(new Fault('network_timeout')), 10_000); }),
  ]).finally(() => clearTimeout(timer));
  requireThat(Array.isArray(results) && results.length > 0 && results.length <= 64 && results.every(r => numericIP(r?.address)), 'network_denied');
  // One immutable numeric address per context: Chromium never resolves the upstream.
  return results[0].address;
}
