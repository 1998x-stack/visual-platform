# ADR-0001：Artifact 独立 Origin 与内容域隔离

- Status: Accepted for MVP
- Date: 2026-11-23

## Context

用户上传的 HTML / JavaScript 是浏览器可执行内容。若与管理平台共享 Origin，恶意脚本可能访问同源 DOM、Storage 或发起同源操作。仅使用路径隔离不是安全边界；不同子域名虽然形成不同 Origin，但 Cookie Domain 仍可能被设置为父域。

## Decision

1. 管理平台与 Artifact 内容使用不同可注册域名（eTLD+1）。
2. 每个 Artifact 使用独立 Host，Host 由服务端登记并映射到 Artifact ID。
3. 内容域只持有 Host-only、__Host- 前缀 Session Cookie；不设置 Domain。
4. 内容请求通过 Static Gateway；Gateway 每次访问都执行授权。
5. 不把 MinIO URL 或长期预签名 GET 作为分享链接。

## Consequences

- 增加 DNS、证书和跨域 SSO Ticket Exchange 的复杂度。
- 降低不同 Artifact 之间的浏览器脚本互相影响的风险。
- 仍需 CSP、权限检查、CSRF 防护、内容策略和安全响应头；Origin 隔离不是所有安全问题的替代品。

## Validation

- 恶意 Artifact 无法读取管理平台 Cookie、DOM、Web Storage。
- Cookie 不包含 Domain 属性，且满足 __Host- 前缀约束。
- 不同 Artifact Host 之间无法读取彼此的 Web Storage。
