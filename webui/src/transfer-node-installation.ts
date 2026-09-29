export function transferNodeTransport(apiURL: string): 'http' | 'https' {
  const scheme = new URL(apiURL).protocol
  if (scheme !== 'http:' && scheme !== 'https:') throw new Error('节点地址必须使用 HTTP 或 HTTPS')
  return scheme === 'http:' ? 'http' : 'https'
}

export interface TransferNodeDockerInput {
  nodeID: string
  token: string
  apiURL: string
  version?: string
  dockerHubNamespace?: string
}

export interface TransferNodeDockerConfiguration {
  compose: string
  reverseProxyRequired: boolean
}

export function transferNodeDockerConfiguration(input: TransferNodeDockerInput): TransferNodeDockerConfiguration {
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(input.nodeID)
    || !/^[A-Za-z0-9_-]{43}$/.test(input.token)
    || !/^(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/.test(input.version || '')
    || !/^[a-z0-9][a-z0-9_-]{0,127}$/.test(input.dockerHubNamespace || '')) {
    throw new Error('Node 发行信息或一次性安装参数无效')
  }

  const transport = transferNodeTransport(input.apiURL)
  const url = new URL(input.apiURL)
  if (!url.hostname || url.username || url.password || url.pathname !== '/' || url.search || url.hash) {
    throw new Error('节点地址必须是 HTTP 或 HTTPS origin')
  }
  // URL.port drops explicitly written default ports such as :80 and :443.
  const authority = input.apiURL.match(/^https?:\/\/([^/?#]+)/i)?.[1] || ''
  const rawPort = authority.match(/:(\d{1,5})$/)?.[1]
  const publicPort = rawPort ? Number(rawPort) : 4433
  if (!Number.isInteger(publicPort) || publicPort < 1 || publicPort > 65535) throw new Error('节点地址端口无效')

  const environment = [
    `      OMC_NODE_ID: ${JSON.stringify(input.nodeID)}`,
    `      OMC_NODE_ENROLLMENT_TOKEN: ${JSON.stringify(input.token)}`,
  ]
  if (transport === 'http') environment.push('      OMC_NODE_TRANSPORT: "http"')

  return {
    compose: [
      'services:',
      '  node:',
      `    image: ${JSON.stringify(`docker.io/${input.dockerHubNamespace}/ohmycine-node:v${input.version}`)}`,
      '    restart: unless-stopped',
      '    ports:',
      `      - ${JSON.stringify(`0.0.0.0:${publicPort}:4433`)}`,
      '    environment:',
      ...environment,
      '    volumes:',
      '      - node-state:/var/lib/ohmycine-node',
      '    stop_grace_period: 60s',
      'volumes:',
      '  node-state:',
      '',
    ].join('\n'),
    reverseProxyRequired: !rawPort,
  }
}
