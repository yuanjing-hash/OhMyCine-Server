import { describe, expect, it } from 'vitest'
import { transferNodeDockerEnvironment, transferNodeTransport } from './transfer-node-installation'

describe('transfer Node Docker installation parameters', () => {
  it.each(['http', 'https'] as const)('retains %s from the saved node address independently of installation commands', scheme => {
    const transport = transferNodeTransport(`${scheme}://node.example.com:4433`)
    expect(transferNodeDockerEnvironment({ nodeID: 'node-1', token: 'fresh-token', transport })).toBe(`OMC_NODE_ID=node-1\nOMC_NODE_ENROLLMENT_TOKEN=fresh-token\nOMC_NODE_TRANSPORT=${scheme}\n`)
  })
  it('rejects line injection into the copied environment', () => {
    expect(() => transferNodeDockerEnvironment({ nodeID: 'node-1', token: 'token\nOTHER=value', transport: 'http' })).toThrow()
  })
})
