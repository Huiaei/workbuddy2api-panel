# 构建与发布说明（本地手工构建）

> **本仓库已关闭 GitHub Actions**（仓库设置 → Actions → 关闭；对应 API `PUT /repos/Huiaei/workbuddy2api-panel/actions/permissions` 传 `{"enabled":false}`）。
> `.github/workflows/` 下的工作流文件已一并删除，**不再有任何自动构建或自动发版**。
> 本页说明如何**完全在本机手工完成**从源码到 Release 的全流程，与被删除的 CI 工作流口径完全一致。

---

## 0. 为什么不用 CI

- 上游 `linguo2625469/workbuddy2api-panel` 已于 2026-10-21 **宣布停止维护**（不再跟进代码、不再处理 issue/PR）。
- 本 fork 的 GitHub Actions 在服务端被单独禁用：即使新建工作流、即使 `workflow_dispatch` 返回
  `422 Actions has been disabled for this repository`，push 事件也**不会产生任何 run**（实测 `actions/runs` 恒为 0）。
- 账号本身无问题（同一账号下其它仓库的定时 run 正常执行，只是它们自己的步骤失败）。
- 因此改为**本机手工构建 + 手工发布**，流程见下文；步骤与原 `go-binaries.yml` 的 `build` / `publish` job 一一对应。

---

## 1. 环境准备

| 项 | 要求 |
|---|---|
| Go | **1.26.10 或更高**（`go.mod` 声明 `go 1.26.10`） |
| 依赖 | 仅 `github.com/redis/go-redis/v9`（可选依赖，REDIS 存储用） |
| 网络 | 默认源可能被墙，建议切国内镜像（见下） |
| Docker | 仅构建镜像时需要（可选） |

```bash
git clone https://github.com/Huiaei/workbuddy2api-panel.git
cd workbuddy2api-panel

# 国内网络建议切源（与 Dockerfile 内一致）
go env -w GOPROXY=https://goproxy.cn,direct
go mod download
```

---

## 2. 编译主程序

`cmd/server` 是网关本体（面板 + 任务体系 + OpenAI 兼容 API）。

```bash
# Linux / macOS 本机
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server

# Windows（PowerShell / cmd）
set CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
```

交叉编译到其它平台（无 CGO，静态链接）：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64  go build -trimpath -ldflags="-s -w" -o dist/wb2api-windows-amd64  ./cmd/server
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64  go build -trimpath -ldflags="-s -w" -o dist/wb2api-linux-amd64    ./cmd/server
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/wb2api-linux-arm64    ./cmd/server
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64  go build -trimpath -ldflags="-s -w" -o dist/wb2api-darwin-amd64   ./cmd/server
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/wb2api-darwin-arm64   ./cmd/server
```

### 2.1 版本串断言（必做）

`cmd/server/main.go` 里的 `appVersion` 是**版本唯一事实源**，面板 `/panel/api/overview` 与发版 tag 都取它。
编译后必须确认二进制里确实带上了这个串——否则就是**编错了树或漏了 ldflags**：

```bash
V=$(grep -oP 'appVersion = "\K[^"]+' cmd/server/main.go)   # 例：1.13.2-panel
for f in dist/wb2api-*; do
  grep -qa "$V" "$f" && echo "OK  $f" || echo "FAIL $f 缺版本串"
done
```

Windows 产物再确认一次 PE 头（**不要在 Windows 上执行它**）：

```bash
file dist/wb2api-windows-amd64
# 期望：PE32+ executable for MS Windows 6.01 (console), x86-64
```

---

## 3. 编译辅助工具

四个 `cmd/*` 都会编译进 Docker 镜像；单独发版时按需编译：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o signin_bin ./cmd/signin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o login       ./cmd/login
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o credit      ./cmd/credit
```

---

## 4. 本地自测（发版前必做）

```bash
go build ./...
go vet ./...

# 关键：CI 环境是 UTC，本机是 CST。带 -count=1 绕开测试缓存，
# 并用 TZ=UTC 暴露「按本地时区构造基线」的用例失败
TZ=UTC go test -count=1 ./...
```

> **时区陷阱**：凡是代码按 `internal/cst.Loc`（UTC+8）计算、而测试用 `time.Local` 构造基线时间的用例，
> 会出现「本机全绿、CI/UTC 全挂」。本仓库已统一把测试里的 `time.Local` 换成 `cst.Loc`，
> 但**新增用例时务必以 `TZ=UTC go test -count=1 ./...` 为准**。

Windows 专项（交叉编译测试二进制，**不执行**）：

```bash
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=windows GOARCH=amd64 go test -c -o /tmp/x.test.exe ./internal/upstream
```

---

## 5. 打包

Windows 用 zip，其余平台用 tar.gz；每个包三件套：**主程序 + `config.example.json` + `README.md`**。

```bash
V=1.13.2                      # 与 appVersion 去掉 -panel 后缀一致
mkdir -p dist && cd dist

# Windows
cp ../dist/wb2api-windows-amd64 wb2api.exe
cp ../config.example.json ../README.md .
zip wb2api-panel-v${V}-windows-amd64.zip wb2api.exe config.example.json README.md
rm -f wb2api.exe config.example.json README.md

# Linux / macOS
for p in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  cp "../dist/wb2api-$p" wb2api
  cp ../config.example.json ../README.md .
  tar czf "wb2api-panel-v${V}-${p}.tar.gz" wb2api config.example.json README.md
  rm -f wb2api config.example.json README.md
done

sha256sum *.zip *.tar.gz > checksums.txt
```

产物命名固定为 `wb2api-panel-v<版本>-<os>-<arch>.<ext>`，与历史 Release 一致。

---

## 6. 打 tag 并发布 Release

### 6.1 版本号必须先改

```bash
# cmd/server/main.go
const appVersion = "1.13.2-panel"
```

tag 名 = `appVersion` **去掉 `-panel` 后缀**，即 `v1.13.2`；不匹配会被 CI 断言拦下——但本仓库已关 CI，**这一条现在靠人工把关**。

### 6.2 推送 tag 与分支

```bash
git add -A && git commit -m "chore: 版本号 1.13.3-panel"
git tag -a v1.13.3 -m "v1.13.3-panel"
git push origin fix/all-known-issues
git push origin v1.13.3
```

### 6.3 创建 Release 并上传资产

```bash
gh release create v1.13.3 \
  --repo Huiaei/workbuddy2api-panel \
  --title "v1.13.3-panel" \
  --notes "见 CHANGELOG；五平台二进制 + checksums.txt" \
  dist/wb2api-panel-v1.13.3-*.zip dist/wb2api-panel-v1.13.3-*.tar.gz dist/checksums.txt
```

没有 `gh` 时用 REST API：

```bash
TOK=<你的 PAT>
REPO=Huiaei/workbuddy2api-panel

curl -s -X POST -H "Authorization: Bearer $TOK" -H "Accept: application/vnd.github+json" \
  "https://api.github.com/repos/$REPO/releases" \
  -d '{"tag_name":"v1.13.3","name":"v1.13.3-panel","body":"见 CHANGELOG","prerelease":false,"draft":false}'

# 上传每个资产（注意 upload_url 要去掉 {?name,label}）
curl -s -X POST -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/octet-stream" \
  "https://uploads.github.com/repos/$REPO/releases/<release_id>/assets?name=" \
  --data-binary @<文件>
```

### 6.4 发布后校验

```bash
# 反向下载自己发的资产，确认 sha256 与本地一致（能抓出「传错文件/传坏文件」）
curl -sL -o /tmp/dl.zip "https://github.com/$REPO/releases/download/v1.13.3/wb2api-panel-v1.13.3-windows-amd64.zip"
sha256sum /tmp/dl.zip
grep windows-amd64 dist/checksums.txt
```

---

## 7. Docker 镜像（可选）

`Dockerfile` 是多阶段构建（`golang:1.26-alpine` → `alpine:3.20`），一次编出四个二进制：

```bash
docker build -t workbuddy2api-panel:1.13.2 .
docker run -d --name wb2api -p 7863:7863 \
  -v "$PWD/auths:/app/auths" -v "$PWD/data:/app/data" -v "$PWD/config.json:/app/config.json" \
  workbuddy2api-panel:1.13.2
```

镜像以 `app` 用户（uid 10001）运行，`/app/auths` 与 `/app/data` 需由该 uid 可写。

---

## 8. 常见问题

| 现象 | 原因与处理 |
|---|---|
| 二进制里找不到版本串 | 编错了树或漏了 `-ldflags "-s -w"`；回到第 2 节重编 |
| Windows 上 `rename` 失败 / 配置保存报错 | 走 `internal/atomicfile` 的原地回退，不要手写 tmp+rename |
| 本机全绿、UTC 全挂 | 时区断言问题，用 `TZ=UTC go test -count=1 ./...` 复现 |
| `go mod download` 超时 | `go env -w GOPROXY=https://goproxy.cn,direct` |
| `git push` 失败但凭据没错 | 多为代理 TLS 抖动，**重试一次**即可 |
