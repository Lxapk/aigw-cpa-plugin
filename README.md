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
| **`a2/b.java:406` 剩余额度** | **`quota.go` / `quota_client.go` / `quota_page.go`** | **`QuotaProvider`** |
| **`A0/s.java:585` 账号选用顺序** | **`pool.go`** | （供上述 hook 共用） |

---

## 2.8 账号轮巡 / 区域选择 / 剩余额度（v0.5.0 新增）

三个机制在源 APK 里是**串在一起**的：

```
额度查询 → 写入账号的 credits → 选号时取 credits 最大者
```

### 2.8.1 区域选择（`a2/b.java:284 D()`）

**判定依据是凭据上的 `domain` 字段**，不是运行时探测：

```java
D(cred) {
  d = cred.domain.toLowerCase()
  if (d.endsWith(".workbuddy.ai") || d == "workbuddy.ai")  return "global"
  else if (d.length() > 0)                                  return "cn"
  else                                                      return 配置默认值
}
```

即：**账号登录时用哪个站点，就永久归属哪个区域。**

### base URL 映射（重要 —— 不是一刀切）

| 用途 | global | cn |
|---|---|---|
| 对话 `/v2/chat/completions` | `www.workbuddy.ai` | **`copilot.tencent.com`** |
| 模型列表 `/console/enterprises/personal/models` | `www.workbuddy.ai` | **`copilot.tencent.com`** |
| 签到 `/v2/billing/meter/daily-checkin` | `www.workbuddy.ai` | **`www.codebuddy.cn`** |
| **额度 `/v2/billing/meter/get-user-resource`** | `www.workbuddy.ai` | **`www.codebuddy.cn`** |
| 登录 state/token | — | `copilot.tencent.com` |
| **Origin/Referer 头** | `www.workbuddy.ai` | **`www.codebuddy.cn`** |

> ⚠️ cn 域下**只有对话和模型列表**走 `copilot.tencent.com`，其余走 `codebuddy.cn`。
> 混用会直接失败。

### 2.8.2 剩余额度

```
POST {base}/v2/billing/meter/get-user-resource
body: {PageNumber:1, PageSize:100, ProductCode:"p_tcaca",
       Status:[0,3], PackageEndTimeRangeBegin/End: now ~ now+3185136000000ms}
```

响应（**注意首字母大写的嵌套**）：

```
data.Response.Data.Accounts[] {
    CycleCapacitySize     周期总容量
    CycleCapacityRemain   周期剩余
    CapacityRemain        回退字段（上面两者都 <=0 时用）
}
```

汇总规则（`a2/b.java:432-450`）：

```
sum = Σ( remain > 0 ? remain : 0 )      // 只累加正数
```

错误处理与源应用一致：

| 情况 | 消息 |
|---|---|
| HTTP 非 2xx | `上游 HTTP <code><body>` |
| 缺 `data` | `响应缺少 data` |
| 其他异常 | `查询额度失败` |

### 2.8.3 账号选用顺序（`A0/s.java:585 t()`）

```java
for (d in accounts) {
    if (d.providerId != providerId) continue
    if (exclude.contains(d.uid)) continue          // 本请求已试过的跳过
    if (!d.disabled && d.enabled && now >= d.untilMillis) {
        if (best == null || d.credits > best.credits) best = d   // ★ 额度最大
    }
}
```

**不是轮询（round-robin），而是「剩余额度最多者优先」。**

> **在 CPA 里的现实**：选哪个凭据由 **CPA 的调度器**决定，插件无法插手
> （插件注册的是 executor，CPA 选好 auth 后才把凭据传进来）。
> 因此插件做的是：把额度读出来用于**展示与排序参考**（面板上的
> 「账号选用顺序」表），并以同样的规则维护自己的账号池视图。

### 失败分类 → 冷却（`W1.b` + `V1/k.java:164,168`）

| 类型 | 触发 | 冷却时长 |
|---|---|---|
| `QUOTA` | 额度耗尽 / 限流 | `quota_cooldown_millis`（默认 12h），**并把 credits 归零** |
| `SOFT` | 瞬时失败 | `soft_cooldown_millis`（默认 60s），连续 `error_threshold` 次后转 `error_cooldown_millis` |
| `ERROR` | 鉴权/硬错误 | `quota_cooldown_millis` |

### 使用

管理面板多了 **「AIGW 额度」** 页：

1. **管理密钥** — 与签到页共用（localStorage）
2. **额度概览** — 「已知额度合计」（无数据时显示 `—`，对应 `N1/R0.java:134`）
3. **刷新** — 「立即刷新全部额度」
4. **自动刷新** — 开关 + 间隔（5-1440 分钟）+ 启动时刷新
5. **账号选用顺序** — 按额度从多到少列出，含冷却类型与可用状态

配置：

```yaml
plugins:
  configs:
    aigw-reverse-proxy:
      quota:
        enabled: true              # 打开定时刷新
        interval_minutes: 30       # 每 30 分钟
        refresh_on_start: true     # 启动时先刷一次
```

HTTP 接口：

```bash
KEY="你的 management key"; BASE="http://127.0.0.1:8317"
P="aigw-reverse-proxy"

curl -s "$BASE/v0/management/$P/quota/status"  -H "Authorization: Bearer $KEY"
curl -s -X POST "$BASE/v0/management/$P/quota/refresh" -H "Authorization: Bearer $KEY"
curl -s -X POST "$BASE/v0/management/$P/quota/config" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"enabled":true,"interval_minutes":30,"refresh_on_start":true}'
```

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
5. 遍历 data.models[]，逐个保留满足以下全部条件的：
     - id 非空
     - id 未出现过（去重）
     - disabled != true
   name 为空时显示名回退为 id；maxInputTokens 映射为 InputTokenLimit
6. agents 中 name=="cli" 的 models 用于**排序**（cli 列出的排前面），
   不再用作白名单
```

> ⚠️ **v0.8.5 修正**：原实现把 `cli` 列表当作**硬性白名单**，导致上游新增
> 但尚未列入 `cli` 的模型被静默丢弃 —— 例如 `deepseek-v4.1-flash`
> （APK 里只有 `deepseek-v4-flash`）。被丢弃后该模型不在目录中，
> 路由便不认领，最终报 `unknown provider for model`。
> 现在 `cli` 列表只用于排序，**所有上游返回的启用模型都会被保留**。

### 模型名处理（`a2/b.java:583 k()`）

```
model = trim(请求里的 model)
if model == "" || model == "auto"  →  用配置的 default_model
```

插件还会额外剥掉 `codebuddy/` 前缀（对应源网关 `V1/o.k` 的显式供应商形式）。

### 路由决策（`model_router.go`）

CPA 需要知道"哪个请求该交给本插件的 executor"。**不认领会直接报
`unknown provider for model <名字>`**（CPA 查不到内置 provider）。

插件按以下顺序判断：

1. 只处理 `chat-completions` 源格式，其他格式一律不管
2. `codebuddy/<model>` 显式前缀 → 认领（并剥掉前缀）
3. `<其他供应商>/<model>` 显式前缀 → **不认领**，让给对应供应商
4. **本插件有可用的 WorkBuddy 凭据？**
   - 没有 → 不认领（交给宿主）
   - 有 → 认领（命中目录时理由为「在目录中」，否则为「有凭据」）

> **第 4 步是 v0.8.4 的关键修正。** 原来只在「模型出现在已缓存目录中」时才认领，
> 而目录是**懒加载**的 —— 若请求先于第一次 `/v1/models` 到达，缓存为空 →
> 不认领 → 报 `unknown provider for model deepseek-v4.1-flash`。
>
> 现在改为以「**本插件是否有该供应商的凭据**」为准（`AvailableProviders` 优先，
> 账号存储兜底）：只要账号存在就认领，真正的非法模型名由上游返回明确错误，
> 这比拒绝路由要好。

**模型目录也会主动拉取**：`model.static` 在缓存为空时会用第一个可用账号
现场查询，而不是返回空列表。

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
| 额度刷新报 401/403 | token 失效，重新登录该账号 |
| 额度显示 `—` | 尚未成功查询过任何账号；点「立即刷新全部额度」 |
| 额度为 0 | 上游返回的 `CycleCapacityRemain` 与 `CapacityRemain` 都 <= 0（额度已用尽） |
| 区域显示不对 | 检查凭据里的 `domain` 字段：以 `.workbuddy.ai` 结尾才算 global（`a2/b.java:284`） |
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

## 2.9 单一界面 + 账号直读（v0.6.0）

### 一个页面装下全部

管理面板的 **「AIGW 反向代理」** 入口现在是一个完整页面，包含：

```
AI 聚合网关 · 反向代理
├─ 管理密钥          （存浏览器 localStorage）
├─ WorkBuddy 账号    ← 账号列表 + 一键「签到 + 刷新额度」
├─ 签到设置          （开关 + 每日时间 + 启动补跑）
├─ 额度刷新设置      （开关 + 间隔 + 启动刷新）
├─ 调用统计          （总调用/今日/失败/Tokens + 最近调用）
└─ 网关设置          （api_key 等只读摘要）
```

顶部有锚点导航，单页内跳转。`/checkin` 与 `/quota` 仍保留为**独立视图**
（方便书签），但内容已包含在主页内。

### 账号登录后立即可见

**之前的实现有缺陷**：账号列表读的是插件自己的**凭据池**，而凭据池只在
「发生请求」或「额度刷新」时才被填充 —— 所以刚登录的账号**要等第一次调用才出现**。

**现在改为直读 CPA 的认证存储**（`host.auth.list`），这是账号的权威来源：

```
登录完成 → saveAuthThroughHost() → 主动失效缓存
                                      ↓
              页面重新渲染时直接列出该账号（无需任何调用）
```

对应源 APK 的行为：它是从自己的账号库（`A0.s`）直接列出的，不依赖流量。

### 只显示 WorkBuddy 账号

严格过滤，**其他供应商（Anthropic / OpenAI / Gemini 等）一律不出现**：

```go
// accounts.go isWorkBuddyAuthEntry
provider 或 type 等于 "codebuddy" 或 "WorkBuddy"      → 收录
auth 文件名以 "codebuddy-" / "codebuddy_" 开头        → 收录
其他                                                  → 过滤掉
```

### 账号状态一目了然

| 列 | 说明 |
|---|---|
| 账号 | 显示名（nickname / label / UID 回退） |
| UID | 供应商侧用户 ID |
| 区域 | `cn` 或 `global`（由凭据 domain 推导，`a2/b.java:284`） |
| 剩余额度 | 已查询则显示数值，否则 `—` |
| 状态 | 可用 / 冷却中（含截止时间）/ 已停用 / 凭据已过期 |

**损坏的凭据文件也会列出来**（状态显示「凭据无法解析」），而不是静默消失 ——
这样你至少知道有这么个文件需要处理。

### 一键操作

页面上的 **「一键：签到 + 刷新额度」** 会依次：

1. 读账号列表
2. 逐个签到
3. 逐个查额度
4. 刷新页面显示结果

对应的 HTTP 接口：

```bash
curl -s -X POST "$BASE/v0/management/aigw-reverse-proxy/run" \
  -H "Authorization: Bearer $KEY"
```

### 账号列表 JSON

```bash
curl -s "$BASE/v0/management/aigw-reverse-proxy/accounts" \
  -H "Authorization: Bearer $KEY"
```

返回 `{accounts, total, usable, credits_known, total_credits, fetched_at, warning}`。

> 列表有 **5 秒缓存**，避免每次渲染都打宿主的 auth 存储。
> 登录成功后会主动失效缓存，所以新账号立刻可见。
> 宿主读取失败时**返回上一次的缓存**并把错误放进 `warning`，页面不会变空。

## 2.10 账号切换策略（v0.7.0）

### 三种策略，面板上一键切换

管理面板的 **「账号切换策略」** 区块：

| 策略 | 行为 | 实现方式 |
|---|---|---|
| **按到期** | 优先使用积分**最快到期**的账号（避免过期浪费） | 插件自己算（8 步门控） |
| **按额度**（默认） | 优先使用**剩余积分最多**的账号 —— 源应用的行为（`A0/s.java:596`） | 插件自己算 |
| **轮巡** | 按顺序轮流使用每个账号 | **委托 CPA 内置调度器** |
| **随机** | 每次随机挑选，避免总是命中同一个账号 | 插件自己算 |

> **轮巡为什么委托给 CPA**：CPA 的内置调度器了解插件看不到的信息（优先级、
> 配额状态、跨插件的账号可见性），且不受插件重载影响。插件通过
> `SchedulerPickResponse.DelegateBuiltin = "round-robin"` 把决定权交回去，
> 比自己维护游标更可靠。

选中后点「应用策略」立即生效，无需重启。另有「重置轮巡位置」把轮巡游标归零。

### 实现原理：CPA 的 `Scheduler` 能力

CPA 正常情况下**自己选账号**。插件通过注册 `Scheduler` 能力接管选号：

```
请求进来
  ↓
CPA 收集所有可用候选账号
  ↓
调用 scheduler.pick(候选列表, 请求上下文)
  ↓
插件按当前策略返回一个 AuthID
  ↓
CPA 用该账号执行请求
```

### 三种策略的算法

```go
// 按额度（A0/s.java:596 的移植）
best = nil
for c in candidates:
    if best == nil || c.Credits > best.Credits: best = c    // 严格 >，同分保持顺序
return best.ID

// 轮巡（按 provider 独立游标）
ids = sorted(candidate ids)
pos = cursor[provider]; cursor[provider] = pos + 1
return ids[pos % len(ids)]

// 随机
return candidates[rng.Intn(len(candidates))].ID
```

### 统一的可用性过滤

三种策略共用同一套筛选，对应 `A0/s.java:596` 的守卫：

```
✓ host 报告的状态不是 disabled / unavailable / failed / invalid / error / expired
✓ host 元数据里没有 disabled: true
✓ 插件凭据池里不在冷却窗口内
✓ 本次请求尚未尝试过（读取 tried_auth_ids）
```

**候选为空时插件返回 `Handled: false`**，把决定权交回 CPA 内置调度器，
不会让请求失败。

### 只接管自己的账号

```go
if provider 是 codebuddy/WorkBuddy       → 接管
if provider 列表里有 codebuddy           → 接管
if 明确指定了别的 provider                → 不接管（交给对应供应商）
if provider 信息为空，但候选都是我们的     → 接管
```

其他供应商的请求**完全不干预**。

### 配置

```yaml
plugins:
  configs:
    aigw-reverse-proxy:
      routing:
        strategy: by_credits    # by_credits | round_robin | random
```

也接受中文/别名：`按额度` / `额度` / `credits`、`轮巡` / `rr`、`随机` / `rand`。

### HTTP 接口

```bash
KEY="你的 management key"; P=aigw-reverse-proxy; BASE="http://127.0.0.1:8317"

# 查看当前策略与选择顺序预览
curl -s "$BASE/v0/management/$P/routing/status" -H "Authorization: Bearer $KEY"

# 切换策略
curl -s -X POST "$BASE/v0/management/$P/routing/config" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"strategy":"round_robin"}'

# 重置轮巡位置
curl -s -X POST "$BASE/v0/management/$P/reset" -H "Authorization: Bearer $KEY"
```

### 面板上的「选择顺序预览」

按当前策略展示**实际会被选用的顺序**，并显示每个账号**已被选中多少次**，
方便验证策略是否按预期工作：

| # | 账号 | 剩余额度 | 已选中次数 |
|---|---|---|---|
| 1 | acct-a@example.com | 900 | 42 |
| 2 | acct-b@example.com | 500 | 40 |
| 3 | acct-c@example.com | 120 | 41 |

## 2.11 基于 workbuddy-switch 的完善（v0.8.0）

参考实现 [changexbc/workbuddy-switch](https://github.com/changexbc/workbuddy-switch)（Rust/Tauri 桌面 App）
后，修正了本插件的**一个实现错误**并补齐了**两块能力**。

### 2.11.1 修正：国际版的域名与路径（我原来写错了）

| 项 | 国内版 (cn) | 国际版 (ai) |
|---|---|---|
| API 基址 | `https://www.codebuddy.cn` | `https://www.workbuddy.ai` |
| **产品域（Origin/Referer）** | `https://www.codebuddy.cn` | **`https://www.codebuddy.ai`** |
| OAuth platform | `workbuddy` | **`workbuddy-ai`** |
| **billing 路径** | `/v2/billing/meter/...` | **`/billing/meter/...`（无 /v2）** |
| 对话 / 模型列表 | `copilot.tencent.com` | `www.workbuddy.ai` |

**我原来的三处错误：**

1. 国际版产品域写成了 `workbuddy.ai`，**正确是 `codebuddy.ai`**
   —— CodeBuddy 工具链把非产品域识别为"自建部署"，会去读企业端点设置
2. OAuth platform 用了 `CLI`，未区分档位
3. billing 路径未按档位切换

**国际版 billing 的 404 回退**（参考实现实测得出）：

```
先试 /billing/meter/xxx
仅当返回 HTTP 404 时才回落 /v2/billing/meter/xxx
401 / 业务错误码 / 传输错误都**不算**路径问题，不换候选
```

> 设计上采用「档位单一事实来源」：所有档位差异集中在 `variant.go`，
> 其它文件不得出现档位相关的字面量。

### 2.11.2 积分模型升级：从"剩余量"到"剩余量 + 到期时间"

**原来**只查一个旧接口，拿到一个总数。
**现在**优先查三个新接口（旧接口作兜底）：

```
POST {base}/billing/meter/get-user-resource-summary          当前周期用量
POST {base}/billing/meter/get-user-resource-paid-packages    付费包
POST {base}/billing/meter/get-user-resource-free-packages    免费包
```

每个资源包解析为：

| 字段 | 说明 |
|---|---|
| `PackageCode` / `PackageName` | 包标识 |
| `Total` / `Remaining` / `Used` | 数量（缺失时互相推算） |
| `ExpireAt` | **到期时间** |
| `Expired` | 已过期 |
| `ExpiringSoon` | **7 天内到期** |

**到期时间的解析规则**（对应参考实现的 `resolve_expire_at`）：

```
1. 优先 DeductionEndTime / deductionEndTime / ExpiredTime / expiredTime
2. 若 CycleEndTime 比它早超过 365 天 → 改用 CycleEndTime
   （官方数据里 2049 这类是长期占位值，真实到期在周期末）
3. 结果距 now 超过 730 天 → 视为「无到期时间」
```

三个接口可能提到同一个包，插件会**去重**，避免总额被重复累加。

### 2.11.3 新增「按到期」选号策略

参考实现的重要洞察：**选剩余最多的账号不是最优的 —— 积分会过期，
应该先用快到期的。**

新增 `by_expiry` 策略，完整移植其 **8 步决策链**：

```
1. 过滤有效候选（查询成功 + 未过期 + 有剩余）；空 → 不切换
2. 目标 = 到期最早者（无到期时间的排最后）
3. 紧迫度：目标到期剩余 > min_urgency_hours → 不切（都还早）
4. 已是目标 → 不切
5. 冷却期：距上次切换 < cooldown_seconds → 不切
6. 存活门控：有会话在跑 → 不切（活进程持旧凭据，切了不生效）
7. 价值过滤：目标剩余 < min_remaining → 不切
8. 防抖动：目标仅比当前早 min_gap_hours 以内 → 不切
否则切换
```

**每一步拒绝都带可读原因**，便于从面板与日志判断行为是否符合预期。

配置：

```yaml
plugins:
  configs:
    aigw-reverse-proxy:
      routing:
        strategy: by_expiry      # 新增：按到期
        cooldown_seconds: 300    # 切换冷却（默认 5 分钟）
        min_gap_hours: 6         # 防抖动：到期差不足 6 小时不切
        min_urgency_hours: 24    # 都还剩 >1 天就不切
        min_remaining: 0         # 目标剩余低于此值不切（0=关闭）
```

### 2.11.4 面板增强

**账号表**新增两列：

| 列 | 说明 |
|---|---|
| **版本** | `cn` / `ai`，一眼区分国内/国际账号 |
| **到期** | `N 天后`；7 天内显示 ⚠️ 并高亮；已过期标红 |

**概览卡**新增「积分即将/已过期」计数。

**选择顺序预览**在按到期策略下按到期时间排序，并显示每个账号的到期天数。

> 已过期的积分**不再被视为可用账号**，与参考实现的 `Candidate::valid` 一致。

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
