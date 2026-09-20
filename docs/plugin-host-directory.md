# v0.3.4 宿主适配：分组账号选择、代理目录与手动测试

STATE Kit v0.3.4 的基础采集、手动前置代理、守护和日志继续支持原版 Sub2API 0.2.7。原版 HostService 只返回账号 ID，不提供名称或代理目录。要在插件配置页显示账号名称、按名字选已有代理，或使用手动查找、代理测试、模型测试和 HTML 预览，需要本页的**宿主改动**；仅上传插件无法增加宿主接口。

## 修改范围

适配严格基于官方 `aea725f2ea644d5592d0bbb1d63b607efa7e200a`（0.2.7），与本仓库 0.2.6 的 `overlay/` 无关，不要混用。

- 增加两个可选 HostService RPC：`ListResources` 返回账号 ID / 名称和可用代理的 ID / 名称 / 协议 / 地址；`ResolveProxy` 仅向插件后端按 ID 返回当前认证 URL。
- 沿用原 OpenAI OAuth 传输插件的能力限制，账号名称范围与原目录一致；状态 JSON、下拉列表不包含代理用户名、密码、Token 或票据。
- 不新增数据库字段、不迁移数据、不修改账号业务代理或调度；手动动作扩展会更新宿主插件页面的 Bridge 和预览权限。
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
  -ldflags='-s -w -X main.Version=0.2.7+statekit-actions -X main.BuildType=release' \
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


## v0.3.3 手动操作扩展

当前补丁在上述资源目录基础上增加 `TransportPlugin.RunAction`、管理员 `POST /admin/plugins/:id/actions`、宿主 UI Bridge 的 `plugin.action`。动作接口沿用管理员与 step-up 校验，仅调用已运行插件，不启动临时进程；被动状态查询不会触发采集或测试。插件能力标记用于区分新版宿主与仅支持目录的旧适配。

宿主前端也需重新构建。HTML 预览通过受限的嵌套沙箱呈现；宿主 CSP 允许 about: 子框架，继续禁止外部连接、表单及顶层导航。每次手动测试可停止，重复动作 ID 不会重复发送模型请求。

## v0.3.4 分组 / 账号选择

当前源码增加被动 `plugin.resources` UI Bridge：插件停止时也能读取 OpenAI OAuth 账号、所属分组和可用代理的元数据。宿主前端读取管理员列表并逐项筛选，只传递 ID、名称、分组 ID 及代理协议 / 地址，不向插件页面传递 OAuth Token、代理密码或票据。账号分页会完整读取。

在「选择需要票据的账号」中先选分组，再选账号并添加；「全部分组」和「未分组」也可选择，已添加账号自动过滤。分组只用于筛选，不会改动 Sub2API 账号归属。新增账号默认关闭，保存或开启前属于表单草稿。

此功能需同步更新本补丁并重建宿主前端；仅上传新插件到旧宿主时保留手动 ID 添加。无需启动插件或发送模型请求即可加载新版目录。
