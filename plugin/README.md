# STATE Kit 独立插件

Sub2API v0.2.7 的开源 OpenAI OAuth transport 插件，提供逐账号 Pro / Team STATE 配置、可选前置代理、动态代理采集、出口 IP 与运行日志、固定业务代理复验、续期和响应异常守护。

请先阅读 [安装与使用说明](../docs/plugin.md)。配置在「插件管理 → STATE Kit → 配置」，基础功能不用修改宿主源码；账号名称和 IP 管理下拉选择需[可选宿主适配](../docs/plugin-host-directory.md)。首次安装需在宿主信任本插件的发布者公钥。

- 插件 ID：`io.github.wangyunjeff.sub2api-state-kit`
- 插件版本：`0.3.2`
- 协议：Sub2API 插件协议 / transport / UI Bridge / HostService v1
- 宿主源码基线：官方 `v0.2.7`，提交 `aea725f2ea644d5592d0bbb1d63b607efa7e200a`
- 当前部署范围：单应用实例
- 默认：所有 STATE 开关关闭，不含任何真实账号或代理配置

## 开发

```bash
go test -race ./...
node --test ui-tests/*.test.cjs
go build -trimpath -o build/state-kit ./cmd/state-kit
```

`internal/pluginapi/v1` 基于上述官方源码的公开契约，追加了可选的 ListResources / ResolveProxy RPC，保留源码和许可归属。其余实现为本项目独立实现，不包含官方闭源传输插件。许可证沿用本仓库 LGPL-3.0。

发布者签名公钥在 `release/`；签名私钥必须在仓库外。参见 `../scripts/package_plugin.py` 和 `integration/` 中的宿主安装/运行集成测试。
