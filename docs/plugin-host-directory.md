# 可选宿主适配：账号名称与 IP 管理

STATE Kit v0.3.2 的基础采集、手动前置代理、守护和日志继续支持原版 Sub2API 0.2.7。原版 HostService 只返回账号 ID，不提供名称或代理目录。要在插件配置页显示账号名称、按名字选已有代理，需要本页的**可选宿主改动**；仅上传插件无法增加宿主接口。

## 修改范围

适配严格基于官方 `aea725f2ea644d5592d0bbb1d63b607efa7e200a`（0.2.7），与本仓库 0.2.6 的 `overlay/` 无关，不要混用。

- 增加两个可选 HostService RPC：`ListResources` 返回账号 ID / 名称和可用代理的 ID / 名称 / 协议 / 地址；`ResolveProxy` 仅向插件后端按 ID 返回当前认证 URL。
- 沿用原 OpenAI OAuth 传输插件的能力限制，账号名称范围与原目录一致；状态 JSON、下拉列表不包含代理用户名、密码、Token 或票据。
- 不新增数据库字段、不迁移数据、不修改账号业务代理、调度或宿主页面。
- 目录每 30 秒刷新；代理在每次采集前重新解析。禁用、过期、删除或解析失败时停止该轮并进入冷却，不会使用直连兜底。
- 代理 ID 参与票据配置绑定；名称改动不使票据失效。代理管理中的认证更新影响后续采集，已有通过业务出口复验的票据可继续使用至原有效期。
- 老宿主返回 Unimplemented 时，插件保留直连和手动填写，禁用代理目录选项。已保存的目录选择不会因临时目录故障被偷偷改为直连。

## 准备可构建宿主

已有官方 0.2.7 Git 源码时，从本仓库根目录运行：

```bash
python3 scripts/prepare_plugin_host.py \
  --upstream /PATH/TO/sub2api \
  --output /PATH/TO/sub2api-directory
```

脚本只从指定官方提交建立新目录并应用补丁，不复制原目录未提交文件、账号、配置或数据库，也不启动服务。输出目录必须不存在。

在新目录按官方构建流程构建前端和后端，例如：

```bash
cd /PATH/TO/sub2api-directory/frontend
corepack pnpm install --frozen-lockfile
corepack pnpm build
cd ../backend
# 前端构建脚本按上游规则复制 dist；确认 internal/web/dist/index.html 存在。
go test ./internal/service -run 'TestPlugin(Resource|Host)|TestBuildHostServices' -count=1
CGO_ENABLED=0 go build -tags embed -trimpath \
  -ldflags='-s -w -X main.Version=0.2.7+statekit-directory -X main.BuildType=release' \
  -o sub2api ./cmd/server
```

这是宿主程序替换，不是只上传插件。先在隔离实例测试，保留旧程序和现有数据备份；替换应用程序并重启应用，沿用原数据库、Redis 和配置。使用 Docker 时需要从适配源码构建宿主镜像，不能仅重拉官方镜像。自定义版本号可能使宿主显示“版本范围兼容但未声明测试”，本适配仍基于上述 0.2.7 提交。

`wire_gen.go` 中的目录注入是本补丁的一部分，重新运行 Wire 后须保留该装配调用。补丁不保证能直接应用到其他上游版本。

## 页面设置

插件管理 → STATE Kit → 配置：

1. 在 IP 管理新增或确认一个可用前置代理。
2. 前置代理选择「选择 IP 管理中的代理」，再按名称选择该代理。
3. 动态代理仍填原动态池 URL；添加账号处会显示名称和 ID。
4. 保存，等待采集及业务出口复验成功，查看运行日志后再调用自己的 API Key。

要回退，先把前置代理改为手动或直连并保存，再恢复旧宿主程序；否则老宿主无法解析保存的代理 ID。该适配没有数据库迁移。
