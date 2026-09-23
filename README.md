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
| **`N1/B` + `V1/k` WorkBuddy 设备码登录** | **`workbuddy_auth.go` / `auth_provider.go`** | **`AuthProvider`** |
| **`a2/b.java:745` 模型目录** | **`model_provider.go` / `workbuddy_client.go`** | **`ModelProvider`** |
| **`a2/b.java:335` 上游对话** | **`executor.go` / `workbuddy_client.go`** | **`ProviderExecutor`** |
| **`V1/o.k` step 6 路由分发** | **`model_router.go`** | **`ModelRouter`** |
| **`a2/b.java:496` 每日签到** | **`checkin.go` / `checkin_client.go` / `checkin_page.go`** | **`ManagementAPI`** |

---

## 2.7 WorkBuddy 每日签到（v0.4.0 新增）

在插件管理面板新增「**AIGW 签到**」页面，支持**手动立即签到**与**每日自动签到**。

### 源 APK 调用链

```
UI「批量签到」(N1/C0290r0.java:148)
  → N1/i1.java:35   engine.h("codebuddy", uid, "checkin", cb)
  → N1/C0284o.java  engine.i(providerId, uid, "checkin", cb)
  → V1/k.java:462   provider.e(account, "checkin", cb)
  → a2/b.java:496   e(account, action, cb)      ← 1375 指令，jadx 反不了，读 smali
```

### 签到接口（smali `a2/b.smali:2643`）

```
POST {checkinBase}/v2/billing/meter/daily-checkin
     body: {}
     checkinBase = domain=="global" ? https://www.workbuddy.ai : https://www.codebuddy.cn
```

> ⚠️ **签到域名与对话域名不同**：cn 域下对话走 `copilot.tencent.com`，
> 而签到走 `www.codebuddy.cn`。两者不能混用。

两个域名都已实测存在（无 token 返回 401）。

### 响应判定（smali 2676-2919 逐行还原）

```
HTTP 非 2xx  →  失败："签到失败（HTTP <status>）：<body>"

HTTP 2xx:
  code = body.code   （缺失/不可解析 → -1）

  code == 0                        →  ✅ "签到成功"
  code != 0 且 body 含 "已签到"
              或 含 "already"      →  ✅ "今日已签到"（幂等，视为成功）
  其它                             →  ❌ message 字段 或 "签到失败（code=N）"
```

与源 APK 完全一致：**"今日已签到"也算成功**，所以重复签到不会报错。

### 手动签到

打开 `http://你的服务器:8317/management.html` → **AIGW 签到**。

页面上有三块：

1. **管理密钥** — 填入 CPA 的 `remote-management.secret-key`，点「保存到浏览器」
2. **自动签到** — 开关 + 每日时间 + 启动补跑
3. **手动签到** — 点「立即为所有账号签到」

> **密钥保存在浏览器本地（localStorage），不会上传到插件或服务器。**
> 这是必要的：CPA 的管理端从 **HTTP 头**鉴权
> （`Authorization: Bearer <key>` 或 `X-Management-Key`，见
> `internal/api/handlers/management/handler.go:276`），而 HTML 表单无法设置请求头。
> 页面因此改用 `fetch()` 携带密钥，密钥始终留在你自己的浏览器里。

填好密钥后：

插件会：

1. 通过 `host.auth.list` 读取 CPA 里所有 `codebuddy` 账号
2. 逐个调用 `daily-checkin`
3. 在页面上列出每个账号的结果（成功 / 已签到 / 失败 + 原因 + code）

> **签到不消耗管理密钥**：签到逻辑本身走的是插件↔宿主的**进程内 RPC**
> （`host.auth.*`）与腾讯 API，只在**页面调用端点**时才需要密钥。

### 自动签到

同一页面配置：

| 项 | 说明 |
|---|---|
| **启用每日自动签到** | 总开关，默认**关闭** |
| **每天 N 时 M 分执行** | 本地时区，默认 09:00 |
| **启动时补跑** | 当天尚未执行过时，插件加载后立即补一次 |

调度由插件内的轻量循环驱动（每分钟探测一次），并用「当天已跑」标记保证
**每天至多执行一次**，不依赖 cron。

### 配置文件写法

```yaml
plugins:
  configs:
    aigw-reverse-proxy:
      # ... 其它配置 ...
      checkin:
        enabled: true          # 打开自动签到
        hour: 9                # 每天 9 点
        minute: 0
        on_start: true         # 启动时补跑
        retry_on_device_fingerprint: true   # 设备指纹失败后 8s 重试一次
```

> 面板上的开关与配置文件是同一份状态：面板保存后会立即生效（启用时自动启动调度器）。

### 关于「设备指纹」重试

源 APK 在签到被上游以 code **9074**（设备指纹未通过）拒绝时，会 `sleep 8s` 后**重试一次**
（`d2/C0482C.java` 的 `catch (w e5) { if (e5.f != 9074) throw e5; Thread.sleep(8000L); ... }`）。
插件保留了这个行为，由 `retry_on_device_fingerprint` 控制，默认开启。

### HTTP 接口（脚本/自动化调用）

```bash
# 查看配置与历史
curl -s http://127.0.0.1:8317/v0/management/aigw-reverse-proxy/checkin/status

# 触发一次手动签到
curl -s -X POST http://127.0.0.1:8317/v0/management/aigw-reverse-proxy/checkin/run

# 修改自动签到配置
curl -s -X POST http://127.0.0.1:8317/v0/management/aigw-reverse-proxy/checkin/config \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"hour":9,"minute":0,"on_start":true}'
```

### 排障

| 现象 | 原因 |
|---|---|
| 「没有可签到的 WorkBuddy 账号」 | CPA 的 auth 存储里没有 codebuddy 账号，先去「认证」页登录 |
| 「读取账号失败」 | `host.auth.list` 不可用（宿主版本过旧） |
| 每个账号都失败 `签到失败（HTTP 401）` | WorkBuddy token 失效，重新登录 |
| `签到失败（code=9074）` | 设备指纹被拒；已自动重试一次仍失败时需重新登录该账号 |
| 自动签到没触发 | 确认 `enabled: true`，且插件在配置的**当天该时刻之后**处于运行状态；可用「启动时补跑」兜底 |
| **点保存/签到后页面一片空白** | v0.4.1 修了表单落到 GET-only resource 路由的问题；v0.4.2 起页面不再用 HTML 表单 |
| **`{"error":"missing management key"}`** | **v0.4.2 已在页面上提供密钥输入框**：填入 CPA 的 `remote-management.secret-key`，点「保存到浏览器」即可。密钥只存在本地浏览器 |
| 用 curl 调端点时报 missing management key | 需要带 `-H "Authorization: Bearer <key>"` 或 `-H "X-Management-Key: <key>"` |

### 管理密钥（v0.4.2）

CPA 的管理端点要求请求头带密钥，而页面是从 resource 路由加载的
（该路由**只接受 GET**）。HTML 表单两者都无法满足，所以页面改为：

```
页面上填密钥 → 存 localStorage → 点按钮时用 fetch() 带上 Authorization 头
```

**密钥全程留在浏览器，不经过插件，也不写入配置文件。**

对应的 curl 等价调用：

```bash
KEY="你的 remote-management.secret-key"
BASE="http://127.0.0.1:8317"

curl -s -X POST "$BASE/v0/management/aigw-reverse-proxy/checkin/run" \
  -H "Authorization: Bearer $KEY" | python3 -m json.tool

curl -s "$BASE/v0/management/aigw-reverse-proxy/checkin/status" \
  -H "Authorization: Bearer $KEY" | python3 -m json.tool
```

> 账号池的**轮换重试**由 CPA 自己的 auth 轮换机制承担；插件负责把源应用的**冷却策略**
> （硬冷却 / 软冷却 / 永久停用）准确地喂给响应 hook。

---

## 2.5 WorkBuddy / CodeBuddy 账号登录（v0.2.0 新增）

源应用里 WorkBuddy 的登录方式在 APK 中是**设备码（DEVICE_CODE）**，
而不是 OAuth 回环 —— 这一点决定了它可以在远程服务器上工作：

```java
// jadxout/sources/Y1/b.java
WEBVIEW_CALLBACK=0, DEVICE_CODE=1, SMS_CODE=2, OAUTH_LOOPBACK=3, NONE=4

// jadxout/sources/a2/b.java:61
public final Y1.b f4226c = Y1.b.f3993e;   // f3993e = DEVICE_CODE
```

### 登录流程（全部在插件内完成）

```
① 插件 POST https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI
   ←  {"code":0,"data":{"state":"<uuid>","authUrl":"https://copilot.tencent.com/login?..."}}

② 你把上面这个 authUrl 在浏览器打开并登录

③ 插件轮询 GET https://copilot.tencent.com/v2/plugin/auth/token?state=<state>
   ←  {"code":11217}                       → 继续等（N1/B.java 的 pending 分支）
   ←  {"code":0,"data":{...credentials}}   → 成功
   ←  {"code":<其他>,"msg":"..."}          → 失败

④ 解析凭据 → 通过 host.auth.save 写入 CPA 账号存储
```

**关键点：全程不需要公网回调地址**，因为轮询是插件主动发起的。
这也是它能在腾讯云服务器上跑通、而 OAuth 回环方式（APK 硬编码 `127.0.0.1:51120`）
不行的根本原因。

### 怎么用

插件注册了 `AuthProvider` 后，CPA 管理面板会**自动出现 WorkBuddy 的登录入口**
（面板从 `auth.identifier` 推导出 `<provider>-auth-url` 按钮）。

1. 打开 `http://你的服务器:8317/management.html`
2. 进入 **认证 / Auth**
3. 找到 **WorkBuddy**（provider key 为 `codebuddy`），点登录
4. 按提示在浏览器打开授权链接并登录
5. 页面会自动轮询，登录完成后账号出现在列表里

### 凭据字段（`a2/b.java:t()`）

| 字段 | 别名 |
|---|---|
| `accessToken` | `access_token` |
| `refreshToken` | `refresh_token` |
| `expiresAt` | `expires_at` |
| `uid` | `userId` / `user_id`（缺失时从 JWT claim 回填） |
| `enterpriseId` | `enterprise_id` / `entId` / `tenantId` / `tenant_id`（同上） |
| `nickname` | `nickName` / `name` |

### 域名切换（`a2/b.java:q()`）

| domain | API 基址 | Origin/Referer |
|---|---|---|
| `global` | `https://www.workbuddy.ai` | `https://www.workbuddy.ai` |
| `cn`（默认） | `https://copilot.tencent.com` | `https://www.codebuddy.cn` |

### 手动粘贴凭据

除了交互式登录，管理面板的「上传认证文件」也支持本插件解析：

- 完整的凭据 JSON（即 `a2/b.java:E()` 写出的格式）
- 或直接粘贴一个裸 access token（插件会从 JWT 里解析 `uid` / `tenant_id` / `exp`）

### token 刷新

按 `a2/b.java:c()` 实现：`POST {base}/v2/plugin/auth/token/refresh`，
并用 `expiresIn` 重算 `expiresAt`。

---

## 2.6 把 WorkBuddy 模型以 OpenAI 格式输出（v0.3.0 新增）

这是本插件的**核心用途**：登录 WorkBuddy 后，把它账号下的模型通过 CPA 暴露成
标准 OpenAI 端点，供任意客户端调用。

### 上游接口（`a2/b.java`）

| 用途 | 方法 | 路径 |
|---|---|---|
| 模型目录 | `GET` | `{base}/console/enterprises/personal/models` |
| 对话 | `POST` | `{base_q}/v2/chat/completions` |

`base` / `base_q` 取决于 `domain`：

| domain | chat / models 基址 |
|---|---|
| `global` | `https://www.workbuddy.ai` |
| `cn`（默认） | `https://copilot.tencent.com` |

> **关键优势**：`/v2/chat/completions` 本身就是 **OpenAI Chat Completions 协议**
> （源码里直接读 `stream` / `model` / `tool_choice`），所以**不需要任何格式翻译层** ——
> 客户端请求体只改 `model` 字段后原样透传，响应也原样返回。

### 模型列表过滤规则（`a2/b.java:745 w()`）

```
1. HTTP 必须 2xx，否则报「模型接口 HTTP <code>」
2. 响应必须是合法 JSON，否则报「模型响应不是合法 JSON」
3. code 必须存在且为 0，否则报「模型接口 code=<code>」
4. data 缺失 → 空列表
5. 取 agents 中 name=="cli" 的 models 作为白名单（为空则代表不限制）
6. 遍历 data.models[]，逐个保留满足以下全部条件的：
     - id 非空
     - id 未出现过（去重）
     - 白名单为空 或 id ∈ 白名单
     - disabled != true
   name 为空时显示名回退为 id；maxInputTokens 映射为 InputTokenLimit
```

### 模型名处理（`a2/b.java:583 k()`）

```
model = trim(请求里的 model)
if model == "" || model == "auto"  →  用配置的 default_model
```

插件还会额外剥掉 `codebuddy/` 前缀（对应源网关 `V1/o.k` 的显式供应商形式）。

### 路由决策（`model_router.go`）

CPA 需要知道"哪个请求该交给本插件的 executor"。插件按以下顺序判断：

1. 只处理 `chat-completions` 源格式，其他格式一律不管
2. `codebuddy/<model>` 显式前缀 → 认领（并剥掉前缀）
3. `<其他供应商>/<model>` 显式前缀 → **不认领**，让给对应供应商
4. 否则查已缓存的模型目录，命中则认领

### 配置项

```yaml
plugins:
  configs:
    aigw-reverse-proxy:
      default_model: "auto"    # 空或 auto 时用这个；留空则用目录里第一个
```

> 模型名 **直接使用 WorkBuddy 原生名**，不做重命名 —— 这样客户端看到什么就能调什么。

### 调用示例

```bash
# 先看有哪些模型（登录后自动从 WorkBuddy 拉取）
curl -s http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer <你在 CPA 配的 api_key>" | python3 -m json.tool

# 调用（model 填上面列出的原生模型名）
curl -N http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <你在 CPA 配的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"<WorkBuddy 的模型名>","stream":true,
       "messages":[{"role":"user","content":"你好"}]}'
```

### 排障

| 现象 | 原因 |
|---|---|
| `/v1/models` 返回 401 | 你没带 CPA 的 api_key。把 `allow_no_key` 设为 `true`，或用 `-H "Authorization: Bearer <api_key>"` |
| `/v1/models` 返回空数组 | 还没登录 WorkBuddy，或登录后模型目录没拉到（看日志；上游需 2xx + `code==0`） |
| 401 `invalid_api_key` | 这是**客户端→CPA** 的鉴权，不是上游问题 |
| 聊天返回上游错误 | 看响应里的 `upstream_status`；401/403 说明 WorkBuddy token 失效，重新登录 |
| **`Unexpected JSON token at offset 5: Expected EOF after parsing, but had :`** | **流式 chunk 带了 SSE 的 `data:` 前缀**。CPA 的出站层按裸 JSON 解析每个 chunk，前缀由 CPA 自己加。v0.3.2 已修复，见下方「流式格式」 |
| **`503 auth_not_found: no auth available (providers=codebuddy, ...)`** | **账号没写进 CPA 的 auth 存储**。见下方「账号落盘」。v0.3.1 已修复两个相关缺陷 |

### 流式 chunk 格式（v0.3.2 修复）

`executor.execute_stream` 返回的每个 chunk **必须是裸 JSON**，不能带 SSE 包装：

```
✅ 正确:  {"id":"cmb-...","choices":[{"delta":{"content":"你"}}]}
❌ 错误:  data: {"id":"cmb-...","choices":[...]}\n\n
❌ 错误:  data: [DONE]\n
```

原因：CPA 只在「插件输出格式 ≠ 客户端请求格式」时才跑翻译器
（`internal/pluginhost/adapters_executors.go:552`）。WorkBuddy 两端都是 `chat-completions`，
所以翻译器被跳过，插件返回的 chunk 会**原样**送到响应写出层 —— 而那一层把每个 chunk
当裸 JSON 解析，`data:` 前缀会直接触发 `Unexpected JSON token at offset 5`。

因此插件现在会：

1. 剥掉每帧的 `data:` 前缀（兼容 `data:` 与 `data: ` 两种写法）
2. 丢弃尾部换行
3. 丢弃 `data: [DONE]` 哨兵（**CPA 自己会补**）
4. 丢弃 `: keep-alive` 心跳与空行
5. 丢弃非 JSON 的垃圾帧（避免整条流被一帧坏数据打断）

`data:` 前缀和 SSE 空行由 CPA 负责添加。

### 账号落盘（v0.3.1 修复）

`auth_not_found` 表示 CPA 找不到可用于 `codebuddy` 的凭据。v0.3.0 有两个缺陷会导致
**即使登录成功、账号也进不去**：

| 缺陷 | 后果 | 修复 |
|---|---|---|
| `host.auth.save` 字段名用错（传 `FileName`/`StorageJSON`，实际要 `name`/`json`） | CPA 校验失败（`json is required`），**文件根本没写** | 改用 `pluginapi.HostAuthSaveRequest` 的 `name`/`json` |
| 凭据 JSON 缺 `"type"` 字段 | CPA 从 `metadata["type"]` 判断账号属于哪个 provider，缺失则归为 `unknown`，**永远匹配不到 codebuddy** | storageJSON 补 `"type":"codebuddy"` |

正确的落盘数据长这样：

```json
{
  "name": "codebuddy-<uid>.json",
  "json": {
    "type": "codebuddy",
    "accessToken": "...",
    "refreshToken": "...",
    "expiresAt": 1893456000,
    "domain": "cn",
    "uid": "...",
    "enterpriseId": "...",
    "nickname": "..."
  }
}
```

**验证账号是否落盘：**

```bash
docker exec 你的CPA容器名 ls -la /app/auths/ 2>/dev/null | grep codebuddy
# 或
docker exec 你的CPA容器名 find / -name "codebuddy-*.json" 2>/dev/null
```

看到 `codebuddy-*.json` 就说明账号已就位，此时模型即可正常调用。

> 如果登录时插件无法写盘，登录响应里会带上「写入 CPA 账号存储失败：...」的提示，
> 凭据仍会返回，便于手动排查。

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
