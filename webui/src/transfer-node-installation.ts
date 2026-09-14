export function transferNodeTransport(apiURL: string): 'http' | 'https' {
  const scheme = new URL(apiURL).protocol
  if (scheme !== 'http:' && scheme !== 'https:') throw new Error('节点地址必须使用 HTTP 或 HTTPS')
  return scheme === 'http:' ? 'http' : 'https'
}

export function transferNodeDockerEnvironment(secret: { nodeID: string; token: string; transport: 'http' | 'https' }): string {
  if (!/^[A-Za-z0-9._:-]+$/.test(secret.nodeID) || !/^[A-Za-z0-9._~-]+$/.test(secret.token)) throw new Error('安装参数无效，请重新生成')
  return `OMC_NODE_ID=${secret.nodeID}\nOMC_NODE_ENROLLMENT_TOKEN=${secret.token}\nOMC_NODE_TRANSPORT=${secret.transport}\n`
}
