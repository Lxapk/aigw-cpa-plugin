# 进行中：限流识别 + 按模型冷却（v0.13.29）

## 目标

1. 正确识别上游限流（上游用 502/server_error 返回，非 429）
2. 限流只冷却**出问题的模型**，账号对其他模型仍可用
3. 冷却到上游给出的重置时刻

## 已完成

### 限流识别（rpc.go）
- 上游真实响应：`HTTP 502` + `{"type":"server_error","code":"internal_server_error"}` + 中文文案
- 新增 `rateLimitPhrases` / `quotaPhrases` / `containsAnyFold`
- `classifyUpstream` 在 `statusCode >= 500` 与 `default` 分支检查文案
- 测试：`TestRateLimitDetectedFromChineseMessage` 等 6 项通过

### 按模型冷却（pool.go）
- `credentialLane` 加 `ModelCooldowns` / `ModelCoolKinds` / `ModelCoolReasons`
- `failureForModel(...)` 在 `kind == failureRate && model != ""` 时只停该模型
- `parkModelLocked` / `lane.modelCooled`
- 测试：`TestThrottleParksOnlyThatModel` 等 5 项通过
- **实测已验证**：deepseek-v4.1-flash 受限后 deepseek-v4-pro 正常返回，账号级冷却为零值

### 重置时间解析（pool.go）
- `resetTimePattern` + `parseUpstreamResetTime`
- 注意：Go 的 `Z07:00` layout 不认 `UTC+8`（只认 `+08:00`/`+0800`），已改为手动算偏移
- 上限受 `QuotaCooldownMillis` 约束；提示已过期则回退配置值

### 流式架构修复（executor.go，v0.13.28 已发布）
- 异步 `go pumpUpstreamStreamIntoHost`，立即返回
- 原因：宿主 emit 缓冲只有 16 槽，消费方要等 executor 返回后才启动 → 同步实现第 17 块卡死
- `stream_id` 必填、初始 header `text/event-stream`、panic 恢复（对齐官方示例）

### 面板可见性（accounts.go）
- `workBuddyAccount` 加 `Cooldown` / `ModelCooldowns` / `ModelCoolReasons` / `LastError` / `Failures`
- `formatModelCooldowns` 填充

## 进行中（未完成）

**疑点**：实测 8561 端口显示 `model_cooldowns=None` 且 `usable=False`（走了账号级冷却），
但单元测试 `TestInterceptResponseParksModelNotAccount` 走真实拦截路径**通过**
（`ModelCooldowns = map[deepseek-v4.1-flash:...]`，`CooldownUntil` 为零值）。

### 已做的排查
- `ResponseInterceptRequest` 有 `Model` / `RequestedModel` 字段，CPA 原生提供
- `interceptResponse` 已把 `req.Model` 传给 `resolveContext`
- `failedModelName(ctx)` 优先 `RequestedModel`，回退 `Model`
- `inflight.put` 只在 `intercept_response.go:125`（流式路径）
- 已加 `[WB-DIAG]` 诊断打印到 `intercept_response.go` 的失败分支
- 已构建并部署到 `/tmp/cpa/run/plugins/workbuddy.so`
- 已重启 CPA，**端口 8571**，进程 id `cpa-diag`

### 下一步
1. `process(action=logs, id=cpa-diag)` 取日志，找 `[WB-DIAG]`
2. 发一次会失败的请求触发诊断：
   ```
   curl -sS -X POST http://127.0.0.1:8571/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":5}'
   ```
3. 看 `model=` / `requested=` / `ctxModel=` 的实际值，定位为何实测路径缺 model
4. 修根因 → 删除诊断代码 → 全量测试 → 实测复验
5. 发布 v0.13.29

## 环境备忘

- 本地 CPA 源码：`/tmp/cpa/CLIProxyAPI`
- 运行目录：`/tmp/cpa/run`（`config.yaml` 的 `port` 每次改以绕开旧进程）
- 测试账号：`codebuddy` 国内 + 国际各一，**国内被上游限流到 2026-09-27 20:03:46 UTC+8**
- 官方参考：`examples/plugin/claude-web-search-router/go/execute_stream.go`（异步流式权威写法）
- 推送到 GitHub：本地代理 7890 常掉线，**改用真实 IP 绕过 fake-ip DNS**
  ```
  git -c http.curloptResolve="github.com:443:140.82.112.4" push origin main
  ```
- 发版流程：改 `pluginVersion` → 提交 → tag → CI → 取 sha256 写 registry.json → 提交推送
- 构建插件：`CGO_ENABLED=1 CC=gcc GOOS=linux GOARCH=arm64 go build -buildmode=c-shared -trimpath -o /tmp/cpa/run/plugins/workbuddy.so .`

## 版本

- 已发布：v0.13.28（流式异步修复）
- 待发布：v0.13.29（限流识别 + 按模型冷却 + 重置时间）
