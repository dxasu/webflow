# webflow

> 把 Jenkins 上"手动点一次"的操作录下来，之后用一行命令重复执行。

`webflow` 是一个用 Go 编写的"浏览器录制 / HTTP 回放"小工具。它会启动一个真实的 Chrome 窗口让你完成登录与点击，背后通过 Chrome DevTools Protocol 抓下你触发的 POST 请求与 cookie，存成一份 `flow.json`。后续就可以用 `webflow replay` 不开浏览器、纯 HTTP 地复现这些操作。

适合的场景：

- Jenkins 触发构建、回滚、改配置等可重复操作
- 需要走 SSO 登录 (飞书 / OAuth / SAML) 的内部系统
- 想脚本化但又懒得逆向接口、找 token 的一切 Web 后台

## 项目布局

```
webflow/
├── cmd/webflow/main.go    # CLI 入口,只调用 webflow.Run(os.Args)
├── run.go                 # package webflow:子命令分发与 usage
├── browser.go             # Chrome / Chromium 检测与一键安装
├── flow.go                # Flow / RecordedRequest / Cookie 数据结构
├── record.go              # record 子命令(chromedp + CDP)
├── replay.go              # replay 子命令(net/http + cookiejar + crumb)
├── list.go                # list 子命令
├── go.mod / go.sum
└── README.md
```

业务实现都放在根包 `webflow` 里，对外只导出一个 `Run(args []string)`，方便在其它 CLI 工具或测试中复用。

## 安装与构建

需要 Go 1.21+。

```bash
go install github.com/dxasu/webflow/cmd/webflow@latest
```

或从源码构建：

```bash
git clone https://github.com/dxasu/webflow
cd webflow
go build -o webflow ./cmd/webflow
```

首次执行 `webflow record` 时，如果检测不到 Chrome / Chromium，会询问是否一键安装：

| 平台 | 自动安装方式 |
|---|---|
| macOS | `brew install --cask google-chrome` |
| Debian / Ubuntu | `sudo apt-get install -y chromium-browser` (失败回退 `chromium`) |
| Fedora / RHEL / CentOS | `sudo dnf install -y chromium` 或 `yum` |
| Arch | `sudo pacman -S --noconfirm chromium` |
| Windows / 其他 | 给出手动下载链接 |

如果机器没有 brew，会提示你先去 <https://brew.sh> 装。

## 使用

```bash
# 1. 录制(默认只录制目标 URL 的 host,过滤静态资源与 Jenkins 轮询)
webflow record https://jenkins.example.com/job/my-service/

# 2. 浏览器里登录、改参数、点 Build,然后回到终端按 Ctrl+C 保存
#    重要:Ctrl+C 之前不要先关浏览器窗口,否则可能丢失 cookie

# 3. 看一下都录到了什么 (省略文件名时默认使用 flow.json)
webflow list
webflow list -v              # 详细模式:打印 headers 与 body 内容

# 4. 回放 (省略文件名时默认使用 flow.json)
webflow replay                       # 全量回放
webflow replay -f "/build"           # 只回放 URL 含 /build 的
webflow replay -i 0,3-5              # 只回放指定索引
webflow replay -d 500ms              # 每个请求间隔 500ms
webflow replay -dry                  # 不真实发送,只打印
webflow replay -insecure             # 跳过 TLS 校验(自签证书)
webflow replay path/to/other.json    # 也可以显式指定其它录制文件
```

## 它是怎么工作的

### 录制

1. 用 `chromedp.ExecAllocator` 启动一个非 headless 的 Chrome，并打开目标 URL。
2. 调用 `Network.enable`，订阅 `Network.requestWillBeSent` 事件。
3. 在事件回调里：
   - 用 `shouldRecord` 过滤静态资源、Jenkins 自身的轮询接口 (`progressiveLog` 等)、第三方上报域名。
   - 用 `buildHostMatcher` 进一步把请求限制在目标 host (含子域)，把 SSO 重定向链等噪音排除掉。
   - 直接从事件里携带的 `PostDataEntries[].Bytes`（base64）解码请求体，**同步**写入内存。这是最稳定的取 body 路径——异步路径在大量并发请求下会因为 Chrome 释放请求体而拿到空 body。
4. 后台 goroutine 每 3 秒调用一次 `Network.getCookies`，"全量替换式"刷新 `flow.Cookies`。这样即便用户先关闭浏览器再 Ctrl+C，最近一次的 cookie 快照仍然在内存里。
5. 收到 `SIGINT` / `SIGTERM` 或 `chromedp ctx` 取消时，再补一次 cookie，写入 `flow.json`。

### 回放

1. 加载 `flow.json`，把 cookie 灌进一个 `cookiejar.Jar`。
2. 默认对每个出现过的 host 调一次 `/crumbIssuer/api/json` 取最新 Jenkins CSRF crumb（原录制里的 crumb 通常已过期）。可以通过 `-crumb=false` 关掉。
3. 按原顺序构造并发出每个请求：
   - 跳过由 `net/http` 自管的 hop-by-hop / 伪 header（`Host`、`Content-Length`、`:authority` 等）和 `Cookie`（由 jar 接管）。
   - 注入新 crumb 头。
   - `CheckRedirect` 设为 `ErrUseLastResponse`，每条请求的真实状态都打印出来，避免 302 把 401 / 403 掩盖。
4. 4xx / 5xx 时打印响应片段，便于定位 (token 过期、参数缺失等)。

## 设计上的几个权衡

- **不做 MITM 代理**：MITM 录制兼容性更好，但需要装根证书，体验差。借助 CDP 的 `Network.requestWillBeSent` 已经够用，且可以拿到 Chrome 内部组装好的请求头，不需要逆向。
- **默认按 host 过滤**：实测一次 Jenkins SSO 登录可能录到 70+ 个请求，其中 60+ 个是飞书 / Google Tag / Sentry 等噪音。SSO 流程让浏览器跑完即可，回放时只需要拿到目标站的 cookie。如果你确实想录全部，加 `-host="*"`。
- **cookie 实时同步而非一次性 dump**：浏览器突然崩溃或用户先关窗口都很常见，3 秒一次的轮询能保证录制结果"最差也只丢最近 3 秒的 cookie 变更"。
- **crumb 自动刷新**：Jenkins 的 CSRF token 与 session 强绑定，原始录制里的 crumb 几乎一定过期。我们用 jar 里的 cookie 重新申请一个，比要求用户手动 patch 更省心。

## 已知约束

- **Session 过期**：cookie 是录制结束时的快照。如果 Jenkins 把你的 session 踢了，重放会 401 / 403，重新录一次即可。
- **极大请求体**：`Network.requestWillBeSent` 里的 `PostDataEntries` 在请求体超过几 MB 时可能被截断。日常 Jenkins 操作 (form data) 完全不会触发。
- **依赖 Chrome / Chromium**：录制必须有一个 CDP 内核浏览器；Safari、纯 WebKit 不支持。回放阶段不依赖浏览器。

## License

MIT
