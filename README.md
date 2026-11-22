# SFT-TUI (`sft` / `sftd`)

基于 **TLS 1.3 双向认证（mTLS）** 与 **SFT/1 二进制多路复用协议** 的跨平台安全文件传输系统，提供纯 Go 无 CGO 静态编译的客户端 `sft` 与服务端 `sftd`，同时内置完整的终端交互界面（TUI）与命令行工具（CLI）。

---

## 核心特性

### 安全与信任模型
- **强加密传输**：强制 TLS 1.3（禁用 TLS 1.2 及以下版本与会话票据），默认采用 Ed25519 密钥对。
- **零隐式信任认证**：服务端严格校验客户端 CA 信任链、叶证书与公钥（SPKI）SHA-256 指纹及有效期；客户端校验服务端 CA 链、SAN（DNS/IP）及可选服务端指纹固定（Pinning）。
- **外部私钥（CSR）签发**：支持客户端本地持有私钥并提交 CSR，服务端仅签发与导出公开证书，私钥始终不离客户端。
- **路径隔离与防符号链接逃逸**：通过跨平台抽象层（`internal/pal`）的目录句柄锚定（`os.Root`）、`O_NOFOLLOW` 锁文件与严格 NFC UTF-8 单级逻辑文件名校验，杜绝路径穿越与符号链接挟持；禁止覆盖已有本地或远端目标文件。
- **连接到期与即时吊销**：支持证书/ACL/限速配置热更新，吊销或证书到期时主动断开既有连接。

### 传输协议与可靠性（SFT/1）
- **单连接多路复用**：单条 mTLS 连接上支持多并发文件流（奇数 `StreamID`）、流级滑动接收窗口、心跳保活（`PING`/`PONG`）与优雅停服通知（`GOAWAY`）。
- **断电持久性与断点续传**：服务端存储引擎与客户端任务队列均基于纯 Go SQLite（`WAL` + `PRAGMA synchronous=FULL`），配合租约隔离、暂存 `.part` 截断对齐与原子硬链接/重命名发布，支持异常掉电或网络中断后的幂等恢复与端到端 SHA-256 完整性校验。
- **精细化资源与限流保护**：全局/单用户上下行令牌桶限速、全局连接上限、每 IP 未认证连接上限、并发 TLS 握手上界、单用户并发流/新建任务频率/检查点频率与存储空间配额。

### 跨平台与交互体验
- **无 CGO 静态构建**：支持 Linux x64 与 Windows x64（含 Windows 命名管道私有 DACL 与 Linux 私有 Unix Domain Socket）。
- **双端 TUI 面板**：
  - `sft` / `sft ui`：本地/远端双栏文件浏览、排序/过滤、多选批量传输、冲突自动重命名/跳过、实时队列测速/ETA、身份切换与连通性测试。
  - `sftd ui`：服务运行概览、在线连接与实时任务监控、用户与权限/带宽策略管理、证书状态与轮换/CSR 签发向导、配置编辑与结构化日志检索。

---

## 目录结构

```text
cmd/
  sft/                 # 客户端入口（TUI + CLI + 本地队列 IPC 客户端）
  sftd/                # 服务端入口（守护进程、证书管理、后台启停、管理 TUI）
internal/
  buildinfo/           # 版本号定义
  certmanage/          # CA/服务端/用户证书签发、轮换、吊销、同步与分发包导出
  config/              # 客户端与服务端 JSON 配置解析、默认值与严格校验
  console/             # 控制台会话与跨平台自动终端启动适配
  control/             # sftd 本地私有 IPC 控制面（热重载、快照、日志、停服排水）
  identity/            # 客户端多身份发现、导入与原子切换
  logfile/             # 带文件锁与大小上限的有界服务日志写入器
  monitor/             # 运行期连接/任务/吞吐快照与有界结构化日志缓冲
  pal/                 # 跨平台安全原语（Linux/Windows 目录锚定、文件锁、IPC、SQLite）
  progress/            # CLI 终端进度条渲染
  prompt/              # 交互式 CLI 问答工具
  protocol/            # SFT/1 二进制帧头、消息编解码与文件名规范校验
  queue/               # 客户端 SQLite 任务队列、调度器、快照与本地控制 IPC
  ratelimit/           # 全局与单用户令牌桶限速、流并发与任务/检查点限频
  security/            # TLS 1.3 mTLS 配置、X.509 生命周期签发与实时授权器
  service/             # 后台进程拉起与健康探测
  session/             # 基于单一 TLS 连接的多路复用器（Mux）、流控、心跳与 GOAWAY
  setup/               # 交互式部署与证书扫描向导
  storage/             # 服务端存储引擎（staging/objects/metadata、租约与崩溃自愈）
  transfer/            # SFT/1 传输引擎（Server、Client、Daemon、QueueRunner）
  tui/                 # Bubble Tea 双端 TUI（client*.go / server*.go / views / modals / form）
```

---

## 构建与测试

要求 Go 1.24+（仓库当前 `go.mod` 使用纯 Go 依赖，无需 CGO）。

```bash
# 默认构建 Linux x64 产物到 bin/linux/{sft,sftd}
make build

# 交叉编译 Windows x64 产物到 bin/windows/{sft.exe,sftd.exe}
make build-windows

# 同时构建 Linux 与 Windows 产物
make build-all

# 运行静态检查与全量竞态测试
make check
make test

# 查看覆盖率报告
make test-coverage
```

---

## 快速上手

### 1. 初始化服务端证书与默认配置

```bash
# 交互式初始化（也可加 -y 按默认值非交互式初始化）
./bin/linux/sftd init -y \
  -config configs/server.local.json \
  -dir certs \
  -name alice \
  -hosts localhost,127.0.0.1,::1
```

初始化会生成：
- 自签名 Ed25519 CA（`certs/ca.pem`、`certs/ca-key.pem`）
- 服务端证书与私钥（`certs/server/<serial>/`）
- 初始用户 `alice` 证书与客户端配置（`configs/client.alice.local.json`）
- 服务端配置（`configs/server.local.json`）

### 2. 启动服务端

```bash
# 方式 A：前台运行（支持 SIGHUP 热重载、Ctrl+C 优雅排空、二次 Ctrl+C 强制退出）
./bin/linux/sftd serve -config configs/server.local.json -log-level info

# 方式 B：后台守护启动
./bin/linux/sftd start -config configs/server.local.json

# 方式 C：打开服务端 TUI 管理面板
./bin/linux/sftd ui -config configs/server.local.json
```

### 3. 客户端传输与 TUI

```bash
# 在交互式终端直接启动客户端 TUI
./bin/linux/sft ui -config configs/client.alice.local.json

# 查看远端已发布文件列表
./bin/linux/sft -config configs/client.alice.local.json list

# 上传 / 下载 / 混合并发传输
./bin/linux/sft -config configs/client.alice.local.json upload ./a.bin ./b.bin
./bin/linux/sft -config configs/client.alice.local.json -out ./downloads download a.bin
./bin/linux/sft -config configs/client.alice.local.json -out ./downloads -save-as renamed.bin download a.bin
./bin/linux/sft -config configs/client.alice.local.json transfer put:./c.bin get:a.bin

# 查询或控制正在运行/待恢复的队列任务（支持通过本地 IPC 控制运行中的 TUI/CLI 实例）
./bin/linux/sft -config configs/client.alice.local.json status
./bin/linux/sft -config configs/client.alice.local.json pause <TASK_ID_PREFIX>
./bin/linux/sft -config configs/client.alice.local.json abort <TASK_ID_PREFIX>
./bin/linux/sft -config configs/client.alice.local.json clean-history
```

---

## 证书管理与运维命令

```bash
# 查看 CA、服务端与用户证书有效期和 SHA-256 指纹
./bin/linux/sftd cert status -server-config configs/server.local.json

# 新建托管密钥用户 / 续期用户证书
./bin/linux/sftd cert user -server-config configs/server.local.json -name bob -days 30
./bin/linux/sftd cert user -server-config configs/server.local.json -name bob -renew -days 30

# 使用外部 CSR 签发用户证书（服务端不接触用户私钥）
./bin/linux/sftd cert user -server-config configs/server.local.json -name carol -csr ./carol.csr -days 30

# 导出用户分发目录（托管密钥用户含配置与密钥；外部密钥用户使用 -public-only）
./bin/linux/sftd cert export -server-config configs/server.local.json -name bob -out ./dist/bob
./bin/linux/sftd cert export -server-config configs/server.local.json -name carol -public-only -out ./dist/carol

# 吊销用户（立即生效并断开活跃连接）
./bin/linux/sftd cert revoke -server-config configs/server.local.json -name bob

# 轮换服务端证书 / 续期 CA（保持 CA 公钥不变，不破坏已有客户端断点身份）
./bin/linux/sftd cert server -server-config configs/server.local.json -hosts localhost,127.0.0.1,::1 -days 30
./bin/linux/sftd cert ca -server-config configs/server.local.json -renew -days 365

# 运行期运维控制
./bin/linux/sftd status -config configs/server.local.json -json
./bin/linux/sftd logs -config configs/server.local.json -level INFO -user alice
./bin/linux/sftd reload -config configs/server.local.json
./bin/linux/sftd stop -config configs/server.local.json          # 广播 GOAWAY 并优雅排空
./bin/linux/sftd stop -config configs/server.local.json -force   # 立即强制断开
```

---

## 配置热更新说明

执行 `sftd reload`、发送 `SIGHUP` 或在 `sftd ui` / `sftd cert` 保存变更时，以下字段支持**不停服热更新**：
- `tls`（CA、服务端证书与私钥）
- `authorized_clients`（授权用户增删、禁用/吊销、权限及单用户带宽策略）
- `global_bandwidth`（全局共享上下行带宽与突发配额）

监听地址（`listen`）、存储根目录（`storage_root`）、连接/流控/超时与存储配额（`limits`、`timeouts`、`resources`）变更需重启 `sftd` 后生效。
