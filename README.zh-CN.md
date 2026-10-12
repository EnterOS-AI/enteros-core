<div align="center">

<p>
  <img src="./docs/assets/branding/molecule-icon.svg" alt="Molecule AI" width="160" />
</p>

<p>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./docs/assets/branding/molecule-text-white.png">
    <img src="./docs/assets/branding/molecule-text-black.png" alt="Molecule AI" width="420" />
  </picture>
</p>

<p>
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<h3>面向异构 AI Agent Workspace 的组织级控制平面</h3>

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-orange.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25+-00ADD8?logo=go)](https://go.dev/)
[![Python Version](https://img.shields.io/badge/python-3.11+-3776AB?logo=python)](https://www.python.org/)
[![Next.js](https://img.shields.io/badge/Next.js-15-black?logo=next.js)](https://nextjs.org/)

<p>
  <a href="./docs/index.md"><strong>文档</strong></a> •
  <a href="./docs/quickstart.md"><strong>快速开始</strong></a> •
  <a href="./docs/architecture/molecule-technical-doc.md"><strong>技术参考</strong></a> •
  <a href="./docs/api-protocol/platform-api.md"><strong>Platform API</strong></a>
</p>

</div>

## 快速开始

```bash
git clone https://git.moleculesai.app/molecule-ai/molecule-core.git
cd molecule-core
./scripts/dev-start.sh
```

打开 [http://localhost:3000](http://localhost:3000)。依赖、手动启动、首次配置
和排障方法见[快速开始指南](./docs/quickstart.md)。

## 本仓库负责什么

`molecule-core` 包含租户侧 workspace server 和 Canvas，提供：

- 经过鉴权的 workspace 生命周期与后端调度；
- 由 `parent_id` 定义、同时用于 peer discovery 与通信授权的组织层级；
- registry、heartbeat、Agent Card、A2A proxy、poll delivery、activity 和
  approval 接口；
- 有 scope 的 agent memory 与 key/value workspace memory API；
- 加密 secrets、files、terminal、templates、bundles、schedules 和运维视图；
- 通过 WebSocket fanout 驱动的 Canvas 实时更新。

Workspace 是持久的组织角色，不是任务节点。团队通过创建 workspace 或调整
`parent_id` 来组合。Canvas 的 **Expand Team View** / **Collapse Team View**
只负责显示或隐藏已有后代，不会创建、停止或删除 workspace。

## Runtime 边界

Agent 执行属于独立维护的 workspace-runtime 和 workspace-template 仓库。Core
负责保存和转发受支持的配置、提供经过鉴权的平台与层级上下文，并把生命周期
操作分派到当前配置的后端。

[`manifest.json`](./manifest.json) 是 Core 当前提供的 template / plugin 仓库清单；
每个条目都固定到不可变 commit。文档不应复制固定的 runtime 数量，也不应使用
可变的 `main` 引用；应直接检查 manifest 和 runtime 自己的 parser。

已经退役的 `shared_context` 父文件注入，以及破坏性的 team expand/collapse
路由，都不是当前 runtime 契约。

## 架构概览

```text
Canvas (Next.js)  <-- HTTP / WebSocket -->  Workspace server (Go / Gin)
                                              |            |
                                           Postgres      Redis
                                              |
                                      当前配置的生命周期后端
                                              |
                                  固定 commit 的 workspace template
                                              |
                                      workspace runtime / agent
```

- Postgres 的领域表是持久化当前状态的权威来源。
- Redis 用于存活状态、缓存和 fanout，不是 workspace 的持久真相源。
- `structure_events` 是 append-only 的部分生命周期历史，不是完整 event source。
- 本地和 control-plane provisioning 都通过共享 dispatcher；tier 不决定云厂商。
- 部署拓扑取决于具体环境。本仓库不承诺统一的 EC2、Railway、Render、ECR、
  Neon 或服务共置架构。

代码对应关系和权威文件见[当前技术参考](./docs/architecture/molecule-technical-doc.md)。

## 仓库结构

| 路径 | 作用 |
|---|---|
| `workspace-server/` | Go API、鉴权、生命周期、registry、层级、A2A、memory、bundle 和后端调度 |
| `canvas/` | Next.js 运维 UI |
| `workspace-server/migrations/` | 持久化 schema |
| `.gitea/workflows/` | 当前 CI、release 与部署自动化 |
| `manifest.json` | 固定 commit 的 template/plugin 清单 |
| `docs/` | 架构、协议、开发与 runbook 文档 |

## 部署与验证

权威 SCM 和自动化位于
[`git.moleculesai.app`](https://git.moleculesai.app/molecule-ai/molecule-core)。
变更通过正常 review、merge 和当前 Gitea Actions workflow 发布；不存在本文档支持的
operator host 或 Railway/Render 一键部署路径。

PR 合并本身不能证明用户可见环境已经更新。必须核对精确 commit 的终态 workflow
结果，并验证对应的 staging 或 runtime 健康面。

## 文档导航

- [文档首页](./docs/index.md)
- [快速开始](./docs/quickstart.md)
- [Core 技术参考](./docs/architecture/molecule-technical-doc.md)
- [Platform API](./docs/api-protocol/platform-api.md)
- [通信规则](./docs/api-protocol/communication-rules.md)
- [Registry 与 heartbeat](./docs/api-protocol/registry-and-heartbeat.md)
- [Event log](./docs/architecture/event-log.md)
- [Runtime config 边界](./docs/agent-runtime/config-format.md)
- [Canvas](./docs/frontend/canvas.md)
- [本地开发](./docs/development/local-development.md)

## License

[Business Source License 1.1](LICENSE)，版权所有 © 2025 Molecule AI。许可证于
2029 年 1 月 1 日转换为 Apache 2.0；完整条款以 LICENSE 文件为准。
