# Managed browser companion (protocol v1)

This source package controls an **end-user acquired CloakBrowser binary** using
Playwright. It does not include or download a browser during npm installation.
No external FlareSolverr is used and no cookies are exported to it.

## Runtime prerequisites

- Node >=20 (release packaging uses Node 22), `npm ci --omit=dev --ignore-scripts`.
- Windows x64, Linux x64 or Linux arm64. Windows arm64 is unsupported.
- Linux Chromium shared libraries and fonts; run non-root with Chromium sandbox
  support enabled. There is deliberately no `--no-sandbox` fallback.
- Writable private data directory, accessible only by the Server account.
- First installation requires a human to review and explicitly accept the
  [upstream binary license](https://github.com/CloakHQ/CloakBrowser/blob/main/BINARY-LICENSE.md).
  The official downloader verifies signed checksums. Current platform pins are
  146.0.7680.177.5 (Windows/Linux x64), 146.0.7680.177.3 (Linux arm64).
  We do not promise support for every site's verification or future entitlement.

Server starts `node browser-companion/src/main.mjs` with private environment:
`OMC_CLOAK_TOKEN` (32–256 URL-safe characters), `OMC_CLOAK_PORT` (default19876),
`OMC_CLOAK_DATA_DIR` (absolute path). These are not browser/user configuration.
Listen is fixed to 127.0.0.1; do not publish this port or raw browser CDP.

## API

All operations are POST JSON, `Authorization: Bearer <token>`, no browser Origin
header. Successful responses are plain JSON, failures `{ "error": "stable_code" }`.
All responses are `no-store`; Server must authenticate/authorize its own UI routes.

| Path | Request | Response |
|---|---|---|
| `/v1/status` | `{}` | `{protocolVersion:1,state,supported,installed,runtimeError}` |
| `/v1/install` | `{licenseAccepted:true}` | status (`installing`); poll status |
| `/v1/shutdown` | `{}` | `{}` after closing browser, then listener exits |
| `/v1/session/create` | `{identity,origin,cookies?}` | `{sessionId,expiresAt}` ISO time |
| `/v1/session/snapshot` | `{identity,sessionId}` | `{imageBase64,mimeType,width,height}` |
| `/v1/session/input` | binding plus `{action,x?,y?,text?,key?,deltaY?}` | `{}` |
| `/v1/session/navigate` | binding plus `{url}` | `{}`; same-origin GET only |
| `/v1/session/request` | binding plus `{url,method,body?,contentType?}` | `{status,headers,bodyBase64,cookies}` |
| `/v1/session/cookies` | binding | `{cookies}`; trusted Host only |
| `/v1/session/close` | binding | `{}` |

States: `not_installed`, `installing`, `ready`, `install_failed`, `launch_failed`.
`installed` is installation metadata, not proof the executable launches. A failed
launch sets `launch_failed` with a safe `runtimeError`; explicit create retries
without reinstalling. No raw exception text is returned. Input actions are
click, text, key, scroll; key values are a fixed navigation/editing allowlist.
No selectors, arbitrary script, raw CDP, file upload, public cookie dump or captcha solver.
`identity` must be an opaque Server-owned binding to actor/plugin/version/connection
and mirror revision, not a caller-chosen untrusted authorization assertion.

Only one session exists at a time, reclaimed after 30 minutes. This is a browser
resource lifetime, not login expiry. The trusted Go Host exports cookies after
validated login/request and encrypts them per connection, then imports them into
a fresh context before first navigation and checks actual authentication.
Only cookies are transferred: at most 128 entries / 32KiB, with exact host-only
domain, bounded name/value/path, expiry, httpOnly, secure and sameSite fields.
LocalStorage, password form values and full Playwright storageState are never
exported. These sensitive protocol results must not reach public APIs or logs.
Operations are serialized and
busy requests fail instead of building an unbounded queue. Text/password values
exist transiently in requests/browser memory only; there is no history or logging.
Snapshot is a private authenticated manual-login view, never a persisted asset.

The main document and Host requests remain bound to one HTTPS mirror.
HTTPS subresources, child frames and dedicated/blob Workers can load
without per-CDN exceptions. The context-private CONNECT proxy validates every
DNS answer as a numeric IPv4/IPv6 address and pins it for the session, including
private, loopback and TUN/Fake-IP answers returned by the Server's DNS.
Limits are 128 pinned hosts, 8 concurrent DNS lookups with a bounded queue and
10-second DNS/queue deadlines, 64 answers per host and 128 sockets. IP-literal
origins and unsafe ports are rejected at proxy level. Successful pins
never change on reload; settled failed resolutions retry only on deliberate
reload. TLS verification, browser CORS and third-party cookie policies remain
enabled. Main-frame cross-origin redirects, service workers, page WebSockets,
downloads and extra pages remain blocked; this is not unrestricted browsing.

`/v1/session/reload` accepts only the session binding and reloads the current
document without replacing the context or cookies. A previous form POST is not
replayed (`browser_reload_post_denied`). Snapshot only updates the picture.
Snapshot adds `networkErrorCode` and `blockedResourceCount` (capped at 10000),
without raw destination URLs. Deliberate reload resets these diagnostics.

The former TUN opt-in environment variable is retired. DNS answers may resolve
to local services in the Server's network, so administrators should only use
trusted mirrors and plugins. Third-party page resources can contact those
services over HTTPS under normal browser rules; the Server does not inject
mirror credentials into other origins. No external DNS/proxy fallback is
introduced. The Server/companion environment itself must have a working route;
a TUN configured only on the administrator's browser does not provide one.

Plugin requests use fixed host-owned `fetch` in that same browser session, not
Playwright's separate HTTP client. GET/POST only, no caller-controlled headers
beyond an allowlisted Content-Type, no followed redirects, maximum 64KiB upload
and 2MiB response. Binary-safe base64 preserves captcha bytes. Only Content-Type
is returned in headers, never Set-Cookie; validated cookies accompany responses
separately for the trusted Host capture boundary. Browsing is
not a general-purpose renderer and should run under OS process/memory limits.

## Verification

`npm test` uses synthetic browser adapters and tests lifecycle, ownership, expiry,
network/address policy, authentication and redaction. It **does not** install,
accept a license, launch Chromium, or prove actual site login works. End-user
installation, sandbox viability and manual verification must be tested in each
deployment platform before claiming working login.
