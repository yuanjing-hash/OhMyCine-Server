import { describe, expect, it } from 'vitest'
import { quarkAuthorizationLabel, quarkAuthorizationURL, quarkPollDelay, quarkQRDataURL } from './quark'

describe('Quark authorization presentation', () => {
  it('distinguishes saved authorization from verified identity', () => {
    expect(quarkAuthorizationLabel({ configured: true, ready: false, state: 'identity_pending' })).toContain('待确认身份')
    expect(quarkAuthorizationLabel({ configured: true, ready: true, state: 'ready' })).toBe('已授权')
  })
  it('only opens official authorization pages and inert image data', () => {
    expect(quarkAuthorizationURL('https://pan.quark.cn/authorize')).toBeTruthy()
    for (const url of ['https://quark.cn.evil.test/', 'javascript:alert(1)', 'https://user:secret@pan.quark.cn/', 'http://pan.quark.cn/']) expect(quarkAuthorizationURL(url)).toBe('')
    expect(quarkQRDataURL('data:image/png;base64,dGVzdA==')).toBeTruthy()
    expect(quarkQRDataURL('data:image/svg+xml;base64,dGVzdA==')).toBe('')
    expect(quarkQRDataURL('https://elsewhere.test/qr')).toBe('')
  })
  it('bounds polling even for invalid provider hints', () => {
    expect(quarkPollDelay(0)).toBe(3000)
    expect(quarkPollDelay(Infinity)).toBe(3000)
    expect(quarkPollDelay(999)).toBe(30000)
  })
})
