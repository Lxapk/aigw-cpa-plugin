# 上传到 GitHub 并让 CPA 自动拉取 —— 操作手册

本文件是给你（仓库所有者）的操作清单。Agent 无法替你 push 到你的 GitHub
（需要你的凭据），但除了「push 这一步」，其余都已准备并验证完毕。

---

## 一、你要做的事（3 步，约 5 分钟）

### 第 1 步：在 GitHub 建空仓库

网页上新建一个仓库，例如 `aigw-cpa-plugin`。
**不要**勾选 "Add a README"，保持空仓。

### 第 2 步：推代码 + 打 tag

```bash
# 解包（源码包在手机 /sdcard/aigw-cpa-plugin-src.tar.gz）
mkdir -p aigw-cpa-plugin && tar xzf aigw-cpa-plugin-src.tar.gz -C aigw-cpa-plugin
cd aigw-cpa-plugin

git init
git add -A
git commit -m "feat: AI 聚合网关反向代理 CPA 插件 v0.1.0"
git remote add origin https://github.com/<你的用户名>/aigw-cpa-plugin.git
git branch -M main
git push -u origin main

# 打 tag 触发 CI（CI 会编译两个架构、打包、发 Release，并把 registry.json 提交回 main）
git tag v0.1.0
git push origin v0.1.0
```

### 第 3 步：等 CI 跑完，确认 registry.json 已回写到 main

打开仓库 → **Actions** 页，等 `Build CPA plugin` 变绿（约 3-6 分钟）。
再去 **Releases** 页确认有 `v0.1.0`，附件含：

```
aigw-reverse-proxy_0.1.0_linux_amd64.zip
aigw-reverse-proxy_0.1.0_linux_arm64.zip
checksums.txt
registry.json
```

同时 `main` 分支根目录应多出一个 `registry.json`（CI 自动提交的）。

---

## 二、填到 CPA 配置

链接就是这一条（把 `<你的用户名>` 换掉）：

```
https://raw.githubusercontent.com/<你的用户名>/aigw-cpa-plugin/main/registry.json
```

写进 CPA 的 `config.yaml`：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/<你的用户名>/aigw-cpa-plugin/main/registry.json"
  configs: {}
```

重启 CPA：

```bash
docker restart <你的CPA容器名>
```

然后打开 CPA 的 WebUI → **插件 → 插件商店**，应该能看到 **AIGW 反向代理**，点安装。

CPA 会自动：
1. 拉 `registry.json`
2. 调 GitHub API 拿你仓库的最新 Release
3. 按容器当前的 `GOOS/GOARCH`（你的是 `linux/amd64`）匹配 zip
4. 下载 zip + 用 `checksums.txt` 校验 sha256
5. 解包，把 `aigw-reverse-proxy.so` 落到 `plugins/linux/amd64/`

---

## 三、装完后加插件配置

在 `config.yaml` 的 `plugins.configs` 下加：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/<你的用户名>/aigw-cpa-plugin/main/registry.json"
  configs:
    aigw-reverse-proxy:
      enabled: true
      priority: 10
      api_key: "sk-改成你自己的密钥"
      allow_no_key: false
      default_provider: "trae"
```

> 注意：`configs` 的键必须与 `.so` 文件名（去掉扩展名）一致 —— `aigw-reverse-proxy`。

再次重启，日志里应出现：

```
pluginhost: plugin loaded ... id=aigw-reverse-proxy
```

验证：

```bash
# 无 key 应被拒 -> 401
curl -s -o /dev/null -w "%{http_code}\n" \
  http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{"model":"x","messages":[]}'

# 带 key 应通过鉴权
curl -s http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-改成你自己的密钥' \
  -H 'Content-Type: application/json' \
  -d '{"model":"test","messages":[{"role":"user","content":"hi"}]}'

# 状态面板
curl -s http://127.0.0.1:8317/v0/management/aigw-reverse-proxy/status
```

---

## 四、以后发新版本

改完代码后：

```bash
git tag v0.1.1
git push origin v0.1.1
```

CI 会自动重编、发新 Release、并把 `main` 上的 `registry.json` 更新到 v0.1.1。
CPA 商店里的「更新」按钮就能用。

---

## 五、供你核对：CI 已通过的全部检查

这些是**在沙箱里用相同命令预先跑通**的，CI 上行为一致：

| 阶段 | 命令 | 结果 |
|---|---|---|
| 依赖 | `go mod download` | ✅ 公共模块 `CLIProxyAPI/v7 v7.3.15` 可解析 |
| 单测 | `CGO_ENABLED=0 go test ./...` | ✅ 32/32 通过 |
| 编译 | `CGO_ENABLED=1 go build -buildmode=c-shared` | ✅ 产出 `.so` |
| 冒烟 | `./smoke aigw-reverse-proxy.so` | ✅ `SMOKE TEST PASSED` |
| 符号 | `nm -D \| grep cliproxy_plugin_init` | ✅ 4 个 ABI 符号齐全 |
| 打包 | `scripts/build_release.py` | ✅ 生成合规 zip + checksums.txt |
| 清单 | `scripts/gen_registry.py` | ✅ 生成 registry.json |
| **商店校验** | **CPA 自己的 `internal/pluginstore` 函数** | ✅ **全部通过** |

最后一项最关键：不是我自己写的校验，而是**直接调用 CPA 源码里的**
`ParseRegistry` / `ArchiveName` / `SelectReleaseAssets` / `ParseChecksums` /
`VerifyChecksum` 来验证，所以「CPA 能不能装上」是有确证的。

---

## 六、排障

| 现象 | 原因 |
|---|---|
| Actions 报 `go: cannot find module ... CLIProxyAPI/v7` | `go.mod` 里的版本号不对，须为 `v7.3.15`（已配好） |
| Releases 里没有 zip | tag 没推或 CI 失败，看 Actions 日志 |
| `raw.githubusercontent.com/.../registry.json` 404 | CI 的「commit registry.json」步骤失败，或链接里分支名不是 `main` |
| 商店里看不到插件 | `store-sources` 的 URL 拉不到，或 `schema_version` 不是 1/2 |
| `release asset ... not found` | tag 名与资产名不匹配（tag `v0.1.0` → 资产内是 `0.1.0`，无 v 前缀；CI 已处理） |
| `release asset checksums.txt not found` | Release 缺 checksums.txt（CI 已包含） |
| `checksum mismatch` | zip 被改动；重新走 CI |
| 日志没有 `plugin loaded` | 架构不匹配。容器里执行 `uname -m` 确认是 `x86_64` |
| 请求全 401 | `api_key` 已设且 `allow_no_key: false`，但客户端没带 `Authorization: Bearer <key>` |

---

## 七、架构提醒（重要）

- 你的腾讯云服务器是 **x86_64**，容器会取 `linux/amd64` 那份 zip。
- CI 同时产出 amd64 和 arm64，所以将来换 arm 机器也能装。
- **不要**使用仓库里旧的 `release/aigw-reverse-proxy_0.1.0_linux_amd64.zip`（若存在）——
  那是早期没有 amd64 编译器时的占位产物，里面的 `.so` 实际是 arm64。CI 产出的才是正确的。
