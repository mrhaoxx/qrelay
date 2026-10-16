# qrelay

Share the QR code in front of your camera with anyone who has the link, in real time. The publisher's browser decodes the code and uploads only the module matrix and text (a few hundred bytes); viewers switch between a module-exact replica and a freshly regenerated code. Single Go binary, no database. Chinese UI; docs below are in Chinese.

把摄像头里捕捉到的二维码实时分享给任何拿到链接的人。**画面不上传**：发布端在浏览器里把二维码解析成模块矩阵和文本，只上传这几百字节。

- **发布端**（`/share/{id}?k=…`）：浏览器打开摄像头，jsQR 定位并解码二维码，取出采样得到的**模块矩阵**（每个黑白格，含可能的误读格）和解码文本，通过 WebSocket 上传。结果变化时立即发送，不变时每秒一条保活。
- **服务端**（Go）：按房间转发；文本变化时用 `go-qrcode` **重新生成**一张干净的二维码下发一次；缓存最新结果给后加入的观看者；每个观看者只保留每类消息的最新一条，慢连接不堆积。
- **观看端**（`/watch/{id}`）：两种显示模式随时切换
  - **像素 1:1 复刻**：按发布端解析出的模块矩阵逐格重绘，和镜头里采样到的一模一样；可选每模块像素数。
  - **重新解析**：显示服务端按文本重新生成的标准二维码。
  - 解码文本（复制 / 打开链接）、版本与矩阵尺寸、冻结画面按钮。
  - **历史内容**：服务端按房间记录出现过的每个不同内容（去重，含首次/最近时间、次数、当时的模块矩阵），观看端点一条即可在两种模式下回看，发布端可清空；`GET /api/rooms/{id}/history` 导出 JSON。
  - 摄像头预览默认关闭；观看者勾选后服务端才通知发布端以 2 fps 上传 240 px JPEG（约 6–10 KB/s），没人要就不传。

## 运行

```sh
go build -o qrelay .
./qrelay                       # http://localhost:8080
./qrelay -self-signed          # https，内存自签名证书，方便手机在局域网内测试
./qrelay -cert fullchain.pem -key privkey.pem -addr :443
./qrelay -secret some-long-secret   # 固定密钥派生种子，重启后发布链接仍然有效
./qrelay -history 200               # 每个房间保留的历史条数（默认 50，0 为不限）
```

所有参数也可以用环境变量设置：`QRELAY_ADDR`、`QRELAY_SECRET`、`QRELAY_HISTORY`、`QRELAY_SELF_SIGNED`、`QRELAY_CERT`、`QRELAY_KEY`。`GET /healthz` 返回 `ok`，`./qrelay -healthcheck` 会探测本机服务并以退出码表示健康状态。

### Docker

```sh
# 现成镜像（CI 发布到 GHCR，amd64 / arm64）
docker run -d -p 8080:8080 -e QRELAY_SECRET=some-long-secret ghcr.io/mrhaoxx/qrelay:latest

# 或自己构建
docker build -t qrelay .
docker run -d -p 8080:8080 -e QRELAY_SECRET=some-long-secret qrelay

# 或用 compose（宿主端口可用 QRELAY_PORT 改，默认 8080）
QRELAY_SECRET=some-long-secret docker compose up -d --build

# 公网部署：加 Caddy 自动申请 HTTPS 证书（域名需已解析到本机，开放 80/443）
DOMAIN=qr.example.com QRELAY_SECRET=some-long-secret docker compose --profile tls up -d --build

# 局域网测试：自签名 HTTPS
QRELAY_SELF_SIGNED=1 docker compose up -d --build
```

镜像基于 `distroless/static`，静态二进制、无 shell、非 root 运行，约 20 MB。自带 `HEALTHCHECK`（调用 `/qrelay -healthcheck`）。自备证书可挂载目录并设置 `QRELAY_CERT` / `QRELAY_KEY`（见 `compose.yaml` 注释）。

浏览器只允许在 **HTTPS 或 `localhost`** 下访问摄像头。想用手机当发布端，请用 `-self-signed`（首次需接受证书警告）或放在支持 TLS 的反向代理后面。

## 流程

1. 打开首页 → 「开始分享」→ 服务端生成房间号和发布密钥，跳到发布页。
2. 发布页显示观看链接（文字 + 二维码），把链接发给对方即可。
3. 观看方打开链接，自己选择「像素 1:1 复刻」或「重新解析」。

房间的发布密钥由 `HMAC(secret, roomID)` 派生，服务端无需保存状态；观看端无需密钥。发布端断开后观看端显示「发布端离线」，但保留最后结果；房间在无人使用 30 分钟后回收。

## 协议

文本帧为 JSON；二进制帧为 `u32 头长度 + JSON 头 + 原始字节`。

| 方向 | 消息 |
|---|---|
| 发布端 → 服务端 | `{"type":"scan","found":true,"text":"…","dim":29,"modules":"<base64>","version":3,"mirrored":false}`，`modules` 为按行、高位在前的位图（1 = 深色） |
| 发布端 → 服务端 | 二进制 `{"type":"preview"}` + JPEG，仅在有观看者请求时 |
| 发布端 → 服务端 | `{"type":"clearHistory"}` |
| 服务端 → 发布端 | `{"type":"status","online":true,"viewers":2}`、`{"type":"demand","preview":false}` |
| 服务端 → 观看端 | `status`、`scan`（加 `seq`、`ts`）、二进制 `{"type":"qr","text":"…"}` + PNG（文本变化时一次）、`preview` 原样转发 |
| 服务端 → 双方 | `{"type":"history","items":[{text,dim,modules,version,first,last,count}]}`，加入时和内容变化时发送完整列表 |
| 观看端 → 服务端 | `{"type":"want","preview":true}` |

## jsQR 补丁

`static/jsQR.js` 是 [jsQR](https://github.com/cozmo/jsQR) 1.4.0 的 UMD 构建（Apache-2.0，许可证见 `static/jsQR.LICENSE`），加了两行补丁：返回值里带上采样出的 `matrix`（BitMatrix）和 `mirrored` 标记。升级时在 `scan()` 的返回对象里加 `matrix: extracted.matrix, mirrored: !!extracted.matrix.mirrored`，并在 `decode()` 的镜像分支前加 `matrix.mirrored = true`。

## 测试

```sh
go test -race ./...
```

## 许可

MIT，见 `LICENSE`。依赖：[jsQR](https://github.com/cozmo/jsQR)（Apache-2.0，随仓库分发并有修改）、[go-qrcode](https://github.com/skip2/go-qrcode)（MIT）、[coder/websocket](https://github.com/coder/websocket)（ISC）。
