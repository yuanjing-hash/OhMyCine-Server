// Companion diagnostics are finite safe codes. Never interpolate raw errors.
export const browserStartupErrors: Readonly<Record<string, string>> = {
  browser_sandbox_unavailable: 'Server 浏览器沙箱不可用。Docker 部署请使用配套浏览器安全配置，并检查宿主机用户命名空间支持；调整部署后重试登录。',
  browser_dependencies_missing: 'Server 浏览器缺少运行依赖。请更新完整安装目录或镜像，并检查 Node 与浏览器共享库。',
  browser_executable_unavailable: 'Server 浏览器文件不存在或无法执行。请检查组件文件和执行权限后重试登录。',
  browser_profile_unavailable: 'Server 浏览器无法建立私有临时目录。请检查服务器临时目录的可写权限和可用空间后重试登录。',
  browser_launch_timeout: 'Server 浏览器启动超时。请检查服务器资源后重试登录。',
}
export function resourceBrowserStartupError(code: string): string | undefined {
  const key = code.startsWith('resource_') ? code.slice('resource_'.length) : ''
  return Object.hasOwn(browserStartupErrors, key) ? browserStartupErrors[key] : undefined
}
