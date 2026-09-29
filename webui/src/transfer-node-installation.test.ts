import { describe, expect, it } from 'vitest'
import { transferNodeDockerConfiguration } from './transfer-node-installation'

const release = {
  nodeID: '24d66f82-43c4-4137-adb4-9a5f3612b294',
  token: 'A'.repeat(43),
  version: '1.1.68',
  dockerHubNamespace: 'laomohouzi',
}

describe('transfer Node Docker Compose', () => {
  it('uses the saved HTTP port and a versioned Docker Hub image without an extra env file', () => {
    const result = transferNodeDockerConfiguration({ ...release, apiURL: 'http://node.example.com:4433' })
    expect(result.reverseProxyRequired).toBe(false)
    expect(result.compose).toContain('image: "docker.io/laomohouzi/ohmycine-node:v1.1.68"')
    expect(result.compose).toContain('"0.0.0.0:4433:4433"')
    expect(result.compose).toContain('OMC_NODE_TRANSPORT: "http"')
    expect(result.compose).toContain(`OMC_NODE_ENROLLMENT_TOKEN: "${release.token}"`)
    expect(result.compose).toContain('node-state:/var/lib/ohmycine-node')
    expect(result.compose).not.toContain('OMC_IMAGE_TAG')
  })

  it('keeps HTTPS as the image default and signals the reverse proxy for an address without a port', () => {
    const result = transferNodeDockerConfiguration({ ...release, apiURL: 'https://node.example.com' })
    expect(result.reverseProxyRequired).toBe(true)
    expect(result.compose).toContain('"0.0.0.0:4433:4433"')
    expect(result.compose).not.toContain('OMC_NODE_TRANSPORT')
    expect(transferNodeDockerConfiguration({ ...release, apiURL: 'https://node.example.com:443' }).compose).toContain('"0.0.0.0:443:4433"')
  })

  it('rejects untrusted release values and line injection', () => {
    expect(() => transferNodeDockerConfiguration({ ...release, apiURL: 'http://node.example.com:4433', token: `${release.token}\nEXTRA=1` })).toThrow()
    expect(() => transferNodeDockerConfiguration({ ...release, apiURL: 'http://node.example.com:4433', dockerHubNamespace: 'other/repository' })).toThrow()
    expect(() => transferNodeDockerConfiguration({ ...release, apiURL: 'http://node.example.com:4433', version: undefined })).toThrow()
    expect(() => transferNodeDockerConfiguration({ ...release, apiURL: 'http://node.example.com:70000' })).toThrow()
  })
})
