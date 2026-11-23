# ADR-0004：MVP 只托管静态 Artifact

- Status: Accepted for MVP
- Date: 2026-11-23

## Context

HTML / CSS / JS 静态资源由浏览器执行；Python / Node / Streamlit / Gradio 等动态应用会在服务器端执行用户代码，风险包括容器逃逸、内网探测、凭证窃取、持久进程和资源耗尽。二者不是同一个运行时模型。

## Decision

MVP 仅托管静态 HTML、CSS、JavaScript、图片、字体和 JSON。服务端扩展名、CGI、脚本执行入口一律拒绝。动态 App 后续使用独立 Runtime Controller，按 App 分配受限容器或 microVM。

## Consequences

- 第一阶段产品能力边界清晰，能够先完成上传、权限、发布和静态访问闭环。
- 用户不能把任意 Python 服务作为静态站点上传后直接运行。
- 第二阶段需要独立威胁模型、网络策略、CPU / RAM / PID / 磁盘配额、镜像治理和运行时回收机制。

## Validation

- 静态 Gateway 从不执行文件系统中的用户脚本。
- 受禁止扩展名的文件在校验阶段被拒绝。
- 动态运行时不与 Static Gateway 共享服务账号或网络权限。
