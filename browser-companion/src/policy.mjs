import ipaddr from 'ipaddr.js';
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
export function publicIP(address) {
  try { return ipaddr.parse(address).range() === 'unicast'; } catch { return false; }
}
export function resourceURL(value) {
  let u;
  try { u = new URL(value); } catch { throw new Fault('network_denied'); }
  requireThat(value.length <= 8192 && u.protocol === 'https:' && !u.username && !u.password && !u.port &&
    !u.hostname.endsWith('.') && !isIP(u.hostname.replace(/^\[|\]$/g, '')), 'network_denied');
  return u;
}
export function tunFakeIP(address) {
  // Deliberately not part of publicIP: only an explicitly trusted TUN route
  // may consume DNS-derived IPv4 benchmark addresses. Never accept mapped IPv6.
  if (isIP(address) !== 4) return false;
  const [a, b] = address.split('.').map(Number);
  return a === 198 && (b === 18 || b === 19);
}
export async function resolvePublic(origin, resolver = lookup, allowTUNFakeIP = false) {
  const hostname = new URL(originURL(origin)).hostname;
  let timer;
  const results = await Promise.race([
    resolver(hostname, { all: true, verbatim: true }),
    new Promise((_, reject) => { timer = setTimeout(() => reject(new Fault('network_timeout')), 10_000); }),
  ]).finally(() => clearTimeout(timer));
  requireThat(Array.isArray(results) && results.length > 0 && results.length <= 64 && results.every(r => publicIP(r.address) || tunFakeIP(r.address)), 'network_denied');
  requireThat(allowTUNFakeIP === true || !results.some(r => tunFakeIP(r.address)), 'tun_fake_ip_requires_opt_in');
  // One immutable numeric address per context: Chromium never resolves the upstream.
  return results[0].address;
}
