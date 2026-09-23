# aigw-reverse-proxy — CLIProxyAPI 反向代理插件

从 Android 应用 **「AI 聚合网关」v0.1.18**（`dev.aigw.app`）中提取其**反向代理网关**实现，移植为
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) v7 的**原生动态插件**（`.so`）。

---

## 1. 源应用的反代实现（逆向结论）

应用本体是个 Shell，网关引擎全部在 `V1` 包：

| 源类 | 职责 |
|---|---|
| `V1/o` extends `h2.AbstractC0573l`（NanoHTTPD） | 网关 HTTP 服务器；`e(Session)` 路由、`j(Session)` 鉴权、`k(Session)` 反代主算法 |
| `V1/k` | 网关引擎：provider 注册表、账号池、调用记账 `r()` |
| `V1/s` | `GatewaySettings` |
| `V1/A` | `Route(providerId, model)` |
| `V1/n` | 单个账号条目（`b`=uid, `c`=label, `e`=disabled, `g`=statusMessage） |
| `V1/C` | 手写 chunked SSE 响应器（含 15s keep-alive 心跳） |
| `V1/m` | 流泵（逐帧解析 usage 并记账） |
| `A0.s` | 账号池健康态：`t()` 选号、`p()` 记失败、`q()` 记成功 |

### 对外路由
```
POST /v1/chat/completions   反向代理入口（OpenAI 兼容）
GET  /v1/models            聚合模型目录
GET  /healthz              存活探针（免鉴权）
GET  /authorize            OAuth 回调（免鉴权）
```
所有响应带 CORS：`Access-Control-Allow-Origin: *`、`Access-Control-Allow-Methods: GET,POST,OPTIONS`、`Access-Control-Allow-Headers: *`。
错误信封：`{"error":{"message":...,"type":"api_error","code":...}}`

### 鉴权（`V1/o.j()`）
1. `settings.allowNoKey == true` → 直接放行
2. `settings.apiKey` 为空 → 拒绝
3. 要求 `Bearer ` 前缀（**忽略大小写**）
4. `MessageDigest.isEqual(token, apiKey)` —— **常量时间比较**

### 反代算法（`V1/o.k()`）
1. 鉴权失败 → `401 invalid_api_key`
2. 读 body（支持 chunked；上限 **8 MB**，8 KB buffer）
   - 空体 → `400 请求体为空`
   - 非 JSON → `400 请求体不是合法 JSON`
3. 取 `stream`、`model`
4. **路由**：
   - `model` 含 `/` 且 `indexOf('/') > 0` → 显式 `provider/model`
   - 否则用 `settings.defaultProvider`
   - 再退到第一个可用 provider
   - 都没有 → `400 缺少 model 参数`
5. 未知 provider → `400 未知供应商：xxx`
6. `provider.k(model)` 模型别名映射，**重写 body 的 `model` 字段**
7. 循环最多 `maxRotate` 次：
   - `A0.s.t(providerId, tried)` 选一个未试过且健康的账号
   - `provider.c(auth, refreshSkewSeconds)` 提前刷新凭据
   - `provider.b(auth, body)` 发起上游请求
   - 响应 `>= 400` → `V1.k.c()` 分类记账并**换号重试**
8. 流式 → `V1/m` 流泵 + `V1/C` 手写 chunked SSE；非流式 → 取全文并解析 `usage`
9. 全部失败 → `503 no_healthy_account`；成功 → `A0.s.q()` 清零错误计数

### 失败分类 → 冷却策略（`V1/k.c()`）
| 情形 | 行为 |
|---|---|
| case 0/1/3（auth / rate / quota） | 立即硬冷却 `quotaCooldownMillis`（默认 12h） |
| case 2 | **永久停用**该账号 |
| case 4/5/6（transient） | 累计连续失败，达 `errorThreshold`（默认 3）才停 `errorCooldownMillis`（默认 10min），否则只停 `softCooldownMillis`（默认 60s） |

### `V1/s` GatewaySettings 默认值（插件 1:1 复刻）
```yaml
port: 8790
api_key: ""
allow_no_key: true
expose_lan: true
only_usable_models: false
refresh_skew_seconds: 86400
max_rotate: 3
quota_cooldown_millis: 43200000   # 12h
soft_cooldown_millis: 60000       # 60s
error_threshold: 3
error_cooldown_millis: 600000     # 10min
log_retention_days: 30
default_provider: "trae"
```

---

## 2. 为什么是「插件」而不是照抄 HTTP 服务器

CPA 已经自带 HTTP 服务器、路由、provider 执行器与凭据池。把原应用的 NanoHTTPD 服务器再抄一遍
既冗余又会和 CPA 抢端口。因此插件**只移植源应用真正独有的那部分逻辑**，其余复用宿主能力：

| 源逻辑 | 插件中的落点 | CPA 能力接口 |
|---|---|---|
| `V1/o.j()` Bearer 常量时间校验 | `frontendauth.go` | `FrontendAuthProvider` |
| `V1/o.k()` step 6-8 路由 + 模型改写 | `routing.go` / `intercept_request.go` | `RequestInterceptor` |
| `V1/o.k()` step 9 失败分类 | `intercept_response.go` | `ResponseInterceptor` |
| `V1/o.p()` + `V1/m` SSE 解析 / usage 累计 | `stream.go` | `StreamChunkInterceptor` |
| `V1/o.r()` 调用记账 | `usage.go` / `usage_handler.go` | `UsagePlugin` |
| `A0.s` + `V1/k.c()` 账号池冷却 | `pool.go` | （供上述 hook 共用） |
| `V1/s` + `AppUiState` 状态面板 | `management.go` | `ManagementAPI` |

> 账号池的**轮换重试**由 CPA 自己的 auth 轮换机制承担；插件负责把源应用的**冷却策略**
> （硬冷却 / 软冷却 / 永久停用）准确地喂给响应 hook。

---

## 3. 获取与构建

### 方式 A：直接下载预编译插件（最快）

从本仓库的 **Releases** 页下载对应架构的 `.so`，文件名统一为 `aigw-reverse-proxy.so`，
按 release 标题区分 `linux/amd64` 或 `linux/arm64`。

下载后直接跳到 [第 4 节](#4-安装到-cliproxyapi)。

> ⚠️ `.so` 是原生库，**必须与 CPA 所在机器的 CPU 架构匹配**。
> 用 `uname -s -m` 确认：`Linux aarch64` → arm64，`Linux x86_64` → amd64。

### 方式 B：从源码构建

```bash
git clone https://github.com/<你的用户名>/aigw-cpa-plugin.git
cd aigw-cpa-plugin

# 依赖：Go 1.26+ 与 C 编译器（cgo 必需 —— 插件是 c-shared 动态库）
go mod download

# 生产插件
CGO_ENABLED=1 go build -buildmode=c-shared -o aigw-reverse-proxy.so .

# 单元测试（32 个用例：默认值/路由/鉴权/冷却/SSE/管理端）
CGO_ENABLED=0 go test ./...

# dlopen 端到端冒烟测试（真实加载 .so 并走完一遍 RPC 会话）
CGO_ENABLED=1 go build -o smoke-bin ./smoke
./smoke-bin aigw-reverse-proxy.so
```

**构建环境要点**：

- **必须有 C 编译器**：`gcc`（Debian/Ubuntu: `apt install build-essential`）或 `clang`。
  用 `CGO_ENABLED=0` 会报 `-buildmode=c-shared requires external (cgo) linking`。
- **不建议交叉编译**：cgo 交叉编译需要目标平台的 C 工具链，很麻烦。
  要跨架构，直接用本仓库已配好的 GitHub Actions（`.github/workflows/build.yml`）——
  打 tag 会自动在 amd64 与 arm64 runner 上分别编译、跑测试与冒烟、并附到 Release。
- **最省事的做法**：直接在跑 CPA 的那台机器上 `go build`。

产物约 7.5 MB，`-buildmode=c-shared`，仅依赖 `libc` / `libresolv`。

导出的 ABI 符号（必须齐全）：

```
cliproxy_plugin_init
cliproxyPluginCall
cliproxyPluginFree
cliproxyPluginShutdown
```

自检：
```bash
nm -D --defined-only aigw-reverse-proxy.so | grep cliproxy_plugin_init
```


---

## 4. 安装到 CLIProxyAPI

CPA 有 **两种**装上插件的方式。

### 方式一：插件商店（推荐 —— 配置里加个链接就能拉）

CPA 内置 plugin store，会从 `registry.json` 清单自动下载并安装插件。
默认源是官方的，用 `store-sources` **追加你自己的源**即可。

**第 1 步：把 Release 资产传上去**

打 tag 推送后，GitHub Actions 会**自动**编出两个架构的包并发布 Release：

```bash
git tag v0.1.0
git push origin v0.1.0
```

Release 上应出现 3 个文件（CI 自动生成，格式由 CPA 校验过）：

```
aigw-reverse-proxy_0.1.0_linux_arm64.zip
aigw-reverse-proxy_0.1.0_linux_amd64.zip
checksums.txt
registry.json
```

> ⚠️ 资产名格式是 CPA 硬性要求：`<id>_<version>_<goos>_<goarch>.zip`，
> 且必须带 `checksums.txt`。改了名字商店就找不到，会报
> `release asset ... not found`。

**第 2 步：把 `registry.json` 放到公网可访问的地址**

最简单是传到你仓库的 `main` 分支根目录，然后用 raw 链接。或者直接用
GitHub Pages / 任意静态托管。

**第 3 步：在 CPA 配置里加源**

```yaml
plugins:
  enabled: true
  dir: "plugins"
  # 追加第三方源（官方源仍是默认内置的，不需要重复写）
  store-sources:
    - "https://raw.githubusercontent.com/<你的用户名>/aigw-cpa-plugin/main/registry.json"
  configs: {}
```

**第 4 步：在 CPA 的 WebUI 里安装**

打开 CPA 管理面板 → 插件 → 插件商店，应该能看到 **AIGW 反向代理**，
点安装即可。CPA 会：
1. 拉 registry.json → 找到插件条目
2. 查你的 GitHub Release → 按当前 `GOOS/GOARCH` 匹配对应 zip
3. 下载 + 校验 `checksums.txt` 里的 sha256
4. 解包 → 把 `aigw-reverse-proxy.so` 落到 `plugins/linux/<arch>/`

装完在 `configs` 下加插件配置（见下方「配置字段」），重启生效。

> 如果源需要认证（私有仓库），用 `store-auth` 配置凭据。

---

### 方式二：手动放置 `.so`

不走商店，直接手动放文件：

```bash
cd /path/to/cpa          # CPA 工作目录（config.yaml 所在目录）
mkdir -p plugins/linux/arm64
cp aigw-reverse-proxy.so plugins/linux/arm64/
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    aigw-reverse-proxy:
      enabled: true
      priority: 10
      api_key: "sk-your-gateway-key"
      allow_no_key: false
      default_provider: "trae"
      max_rotate: 3
      error_threshold: 3
```

**注意**：`configs` 里的键必须与 `.so` 文件名去掉扩展名后完全一致（`aigw-reverse-proxy`）。
文件名不能改。

---

### 配置字段

两种方式共用同一套字段（对应源应用 `V1/s`）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `port` | 8790 | 源应用监听端口；**仅展示**，实际端口由 CPA 决定 |
| `api_key` | `""` | 客户端 Bearer 令牌 |
| `allow_no_key` | `true` | 免鉴权放行（生产建议 `false`） |
| `expose_lan` | `true` | 仅展示 |
| `only_usable_models` | `false` | 仅展示 |
| `refresh_skew_seconds` | 86400 | 提前刷新凭据的秒数 |
| `max_rotate` | 3 | 每请求最大换号次数 |
| `quota_cooldown_millis` | 43200000 | auth/rate/quota 拒绝后的硬冷却（12h） |
| `soft_cooldown_millis` | 60000 | 单次瞬时失败的软冷却（60s） |
| `error_threshold` | 3 | 连续失败多少次才停用 |
| `error_cooldown_millis` | 600000 | 达阈值后的停用时长（10min） |
| `log_retention_days` | 30 | 日志保留天数 |
| `default_provider` | `"trae"` | 无 `provider/` 前缀时使用的供应商 |
| `enforce_default_provider` | `false` | **插件新增**：只允许默认供应商 |
| `debug` | `false` | 详细日志 |

---

### 客户端调用

```bash
# 默认供应商
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer sk-your-gateway-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}'

# 显式指定供应商（前缀会被插件剥掉）
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer sk-your-gateway-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

### 状态面板

```
/v0/resource/plugins/aigw-reverse-proxy/status
```
- `Accept: text/html` → 可视化面板（调用统计 / 网关设置 / 账号池冷却 / 最近调用）
- 否则 → JSON

附加管理路由：
```
GET /v0/management/aigw-reverse-proxy/status   # 同上 JSON
GET /v0/management/aigw-reverse-proxy/calls    # 最近 50 条调用记录
```

### 排障

| 现象 | 原因 |
|---|---|
| 商店里看不到插件 | `store-sources` 的 URL 不可达，或 `registry.json` 格式错（`schema_version` 必须为 2） |
| `release asset ... not found` | Release 资产名不符合 `<id>_<version>_<goos>_<goarch>.zip` |
| `release asset checksums.txt not found` | Release 里没传 `checksums.txt` |
| `checksum mismatch` | zip 被改动过，或 `checksums.txt` 未同步更新 |
| `dynamic library filename must be ...` | zip 内的 `.so` 名字不对或不在根目录 |
| 日志没有 `plugin loaded` | 架构不匹配（用 `uname -m` 核对）、或 `plugins.enabled` 没开 |
| 请求全部 401 | `api_key` 已设且 `allow_no_key: false`，但客户端没带 `Authorization: Bearer <key>` |


---

## 5. 验证状态

| 项目 | 结果 |
|---|---|
| 编译 `c-shared` .so | ✅ `dist/aigw-reverse-proxy.so` |
| ABI 符号导出 | ✅ `nm -D` 四个符号齐全 |
| 单元测试 | ✅ 32/32 通过 |
| `dlopen` + `cliproxy_plugin_init` | ✅ 返回 0，abi_version=1 |
| `plugin.register` | ✅ 15 个 ConfigFields + 6 项 capabilities |
| `frontend_auth.authenticate` | ✅ 无 key → 401 `invalid_api_key`；正确 key → 通过 |
| `request.intercept_before` | ✅ `openai/gpt-4o` → provider=openai, model=`gpt-4o`，stream 保留 |
| `response.intercept_after` | ✅ 记账成功，usage 计数正确 |
| `response.intercept_stream_chunk` | ✅ header-init 正常 |
| `usage.handle` | ✅ 凭据登记 + usage 累计 |
| `management.handle` | ✅ port 9100 生效、api_key 脱敏、calls=2、tokens=24/14、accounts=1 |
| `cliproxy_plugin_shutdown` | ✅ 干净返回 |

> 说明：以上为**插件侧 ABI 级端到端验证**（真实 `dlopen` 加载 .so 并完整跑通 RPC 会话），
> 尚未在真实 CPA 进程 + 真实上游账号下端到端跑通一次实际模型调用。

---

## 6. 源码结构

```
aigw-cpa-plugin/
├── go.mod                    模块 github.com/taixu/aigw-reverse-proxy
├── cabi.go                   C ABI（dlopen 入口 + 缓冲区管理）
├── rpc.go                    RPC 分发 + 注册元数据 + 上游失败分类
├── settings.go               gatewaySettings（V1/s 复刻）+ YAML 解码
├── routing.go                resolveRoute / rewriteModelBody（V1/o.k 6-8）
├── frontendauth.go           常量时间 Bearer 校验（V1/o.j）
├── pool.go                   账号池 + 冷却策略（A0.s + V1/k.c）
├── intercept_request.go      路由改写 + 头戳记
├── intercept_response.go     失败分类 + 记账（V1/o.k 9 + V1/o.r）
├── stream.go                 SSE 解析 + usage 累计（V1/o.p + V1/m）
├── usage.go                  callRecord / callLog / usagePayload
├── usage_handler.go          UsagePlugin 钩子
├── management.go             /status JSON + HTML 面板
├── yaml.go                   配置解码封装
├── plugin_test.go            32 个单元测试
├── smoke/main.go             dlopen 端到端冒烟测试
├── scripts/build_release.py  打包插件商店所需的 zip + checksums.txt
├── scripts/gen_registry.py   生成插件商店用的 registry.json
├── .github/workflows/build.yml   CI：多架构编译 + 测试 + 冒烟 + 打包 Release
└── .gitignore
```

---

## 7. 已知差异与取舍

1. **`port` 字段**：源应用自己监听 8790；插件由 CPA 监听，该字段仅作展示，不参与绑定。
2. **`expose_lan` / `only_usable_models`**：仅登记在状态面板，实际绑定与模型可见性由 CPA 控制。
3. **`enforce_default_provider`**：插件新增开关（源应用总是接受显式 `provider/` 前缀），默认关闭。
4. **账号池轮换次数**：源应用在网关内硬控 `maxRotate`；插件改为把冷却策略交给 CPA 的
   轮换机制，`max_rotate` 仅用于展示。若需要严格等价，可在 CPA 的 provider 配置里限制重试次数。
5. **出站代理 `V1/z` ProxySettings**：源应用另有出站 HTTP/SOCKS 代理设置（host/port/user/pass/
   excludedProviderIds），与「反向代理」无关，插件未包含；CPA 自身支持 `proxy-url` 配置。

---

## 8. License

插件代码为本项目原创移植实现。CLIProxyAPI 的 SDK（`sdk/pluginapi`、`sdk/pluginabi`）
按其仓库 License 使用。
