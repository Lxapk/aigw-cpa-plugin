# PROGRESS — AI 聚合网关反代功能 → CLIProxyAPI 插件

## 目标
从 `/sdcard/AI 聚合网关_0.1.18.apk` 提取「反向代理」功能实现，写成 CLIProxyAPI (CPA) 插件。

## 源 APK 逆向结论（已完成）

APK 真实路径：`/sdcard/AI 聚合网关_0.1.18.apk`（用户最初给的 `/storage/emulated/0/...` 不存在）
md5: `17f8cc23ad0196ddf00c61237793221e`
包名：`dev.aigw.app`
反编译产物：`/workspace/ai_gw/jadxout/`（JADX）+ `/workspace/ai_gw/dec/`（apktool smali）

核心类：
| 类 | 作用 |
|---|---|
| `V1/o` extends `h2.AbstractC0573l` (NanoHTTPD) | 网关 HTTP 服务器；`e(Session)` 路由，`j(Session)` 鉴权，`k(Session)` 反代主算法 |
| `V1/k` | 网关引擎（provider 注册表 + 账号池 + 记账 `r()`） |
| `V1/s` | GatewaySettings |
| `V1/z` | ProxySettings（出站 HTTP 代理，与「反代」无关） |
| `V1/A` | Route(providerId, model) |
| `V1/n` | 账号条目（b=uid, c=label, e=disabled, g=statusMessage） |
| `V1/C` | 手写 chunked SSE 响应器 + 15s keep-alive |
| `V1/m` | 流泵 |
| `A0.s` | 账号池健康态（t() 选号、p() 记失败、q() 记成功） |

路由：`POST /v1/chat/completions`（反代入口）、`GET /v1/models`、`GET /healthz`、`GET /authorize`
响应统一 CORS：`Access-Control-Allow-Origin: *` / `Methods: GET,POST,OPTIONS` / `Headers: *`
错误信封：`{"error":{"message","type":"api_error","code"}}`

`V1/s` GatewaySettings 默认值（已在插件中 1:1 复刻）：
```
port=8790  apiKey=""  allowNoKey=true  exposeLan=true  onlyUsableModels=false
refreshSkewSeconds=86400  maxRotate=3
quotaCooldownMillis=43200000(12h)  softCooldownMillis=60000(60s)
errorThreshold=3  errorCooldownMillis=600000(10m)
logRetentionDays=30  defaultProvider="trae"
```

`V1/o.j()` 鉴权：`allowNoKey` 短路 → `apiKey` 为空则拒 → 要求 `Bearer `（忽略大小写）前缀 → `MessageDigest.isEqual` 常量时间比较。

`V1/o.k()` 反代算法：
1. 鉴权失败 → 401 `invalid_api_key`
2. 读 body（chunked / content-length，上限 8MB，8KB buffer）；空 → 400「请求体为空」；非 JSON → 400「请求体不是合法 JSON」
3. 取 `stream`、`model`
4. 路由：`model` 含 `/` 且 `indexOf('/')>0` → 显式 `provider/model`；否则用 `defaultProvider`；再退到第一个可用 provider；都没有 → 400「缺少 model 参数」
5. 未知 provider → 400「未知供应商：xxx」
6. `provider.k(model)` 模型映射，**重写 body 的 model 字段**
7. 循环 `maxRotate` 次：`A0.s.t(providerId, tried)` 选号 → `provider.c(auth, skew)` 刷新凭据 → `provider.b(auth, body)` 发起上游 → `>=400` 则 `V1.k.c()` 分类记账并换号
8. 流式 → `V1/m` 流泵 + `V1/C` 手写 chunked SSE；非流式 → `provider.k()` 全文 + 解析 usage
9. 全失败 → 503 `no_healthy_account`；成功 → `A0.s.q()` 清错误计数

`V1/k.c()` 失败分类 → 冷却：
- case 0/1/3（auth/rate/quota）→ 立即硬冷却 `quotaCooldownMillis`
- case 2 → 永久停用（`disabled=true`）
- case 4/5/6（transient）→ 累计连续失败，达 `errorThreshold` 才停 `errorCooldownMillis`，否则只是 `softCooldownMillis`

## CPA 插件体系结论（已完成）

仓库：`/workspace/cliproxyapi_ref`（CLIProxyAPI v7，module `github.com/router-for-me/CLIProxyAPI/v7`，要求 Go 1.26）

- 插件 = **原生 C-ABI 动态库 .so**，dlopen 加载
- 导出符号：`cliproxy_plugin_init(host*, plugin*)` / `cliproxyPluginCall(method, req, len, resp)` / `cliproxyPluginFree` / `cliproxyPluginShutdown`
- ABIVersion=1，SchemaVersion=6
- 发现规则：`plugins/<goos>/<goarch>/<id>[-v<ver>].so` 或 `plugins/<id>.so`
- 配置：`config.yaml` → `plugins.enabled/dir` + `plugins.configs.<id>.{enabled,priority,...}`
- RPC 信封：`{"ok":true,"result":...}` / `{"ok":false,"error":{code,message,http_status,retryable}}`
- 关键方法：`plugin.register/reconfigure/quiesce/shutdown`、`frontend_auth.identifier/authenticate`、
  `request.intercept_before/after`、`response.intercept_after`、`response.intercept_stream_chunk`、
  `usage.handle`、`management.register/handle`、`host.log`、`host.auth.*`
- 参考骨架：`examples/plugin/simple/go/main.go`

## 插件工程（已完成实现 + 编译通过）

路径：`/workspace/aigw-cpa-plugin`
模块：`github.com/taixu/aigw-reverse-proxy`（replace → `../cliproxyapi_ref`）
产物：`dist/aigw-reverse-proxy.so`（7.5MB，c-shared，仅依赖 libc/libresolv）
ABI 符号已用 `nm -D` 验证导出。

| 文件 | 内容 | 源 APK 对应 |
|---|---|---|
| `cabi.go` | C ABI + dlopen 入口 | — |
| `rpc.go` | RPC 分发 + 注册元数据 + 错误分类 | — |
| `settings.go` | gatewaySettings + 默认值 + YAML 解码 | `V1/s` |
| `routing.go` | `resolveRoute` + `rewriteModelBody` | `V1/o.k()` step 6-8 |
| `frontendauth.go` | 常量时间 Bearer 校验 | `V1/o.j()` |
| `pool.go` | 账号池 + 冷却策略 + 轮换选号 | `A0.s` + `V1/k.c()` |
| `intercept_request.go` | 路由改写 + 头戳记 | `V1/o.k()` |
| `intercept_response.go` | 失败分类 + 记账 | `V1/o.k()` step 9 + `V1/o.r()` |
| `stream.go` | SSE 解析 + usage 累计 | `V1/o.p()` + `V1/m` |
| `usage.go` / `usage_handler.go` | 调用日志 + 统计 | `V1/f2.C0541a` + `V1/o.r()` |
| `management.go` | `/status` JSON + HTML 状态页 | `V1/s` + `N1.C0270h AppUiState` |
| `plugin_test.go` | 30+ 单测 | — |

## 构建方法（可复用）

```bash
export PATH=/opt/go/bin:$PATH
cd /workspace/aigw-cpa-plugin
CGO_ENABLED=1 CC=cc go build -buildmode=c-shared -o dist/aigw-reverse-proxy.so .
```
注意：沙箱无 `gcc`，用 `/usr/local/bin/cc`（NDK clang 21，target `aarch64-linux-gnu`，glibc 可用）。
Go 装在 `/opt/go`（1.27.1，本任务首次下载安装）。

## 剩余待办

1. **【进行中】** 已给 `gatewaySettings` 补齐 `yaml:` tag（yaml.v3 不认 `json:` tag，之前导致配置全部解码失败）→ **需重跑单测确认**
2. 修 3 个失败用例：
   - `TestLifecycleConfigOverride`（应随 #1 修复）
   - `TestFrontendAuthAllowNoKeyBypasses` / `TestFrontendAuthAcceptsCorrectKeyCaseInsensitiveScheme`（应随 #1 修复）
   - `TestInterceptRequestEnforceDefaultProvider`（enforce 分支未命中，需查 `knownProviders` 是否拿到 provider 列表）
   - `TestHandleUsageRecordsTotals`（凭据未登记 — 检查 `rec.AuthIndex` 字段是否被 CPA 填充）
   - `TestManagementRegistrationAndStatus`（api_key 未脱敏，应随 #1 修复）
3. 端到端验证：dlopen 冒烟测试 或 用 CPA 真实加载 .so
4. 写 README（配置示例 + 安装步骤）
