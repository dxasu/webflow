// Package webflow 提供 "录制 / 回放" Web 操作 (尤其是 Jenkins 构建)
// 的核心能力,以一组子命令的形式对外暴露:
//
//   - record: 启动 Chrome,让用户自行登录并操作,实时抓取 POST 请求与 cookie
//   - list:   把录制结果以人类可读的方式展示出来
//   - replay: 用 net/http 复发录制中的请求,并自动刷新 Jenkins CSRF crumb
//
// 该包对外只导出一个 [Run] 函数。CLI 入口位于 cmd/webflow,
// 业务实现保留在本包内,便于在其它工具或测试中复用。
package webflow

import (
	"fmt"
	"os"
)

// Run 是 webflow 的子命令分发器,典型调用形式为 webflow.Run(os.Args)。
// 参数 args 与 os.Args 同构: args[0] 为程序名,args[1] 为子命令,余下为子命令参数。
//
// 不会返回错误。遇到无法继续的情形时,通过 os.Exit 终止进程,
// 退出码 0 表示成功,2 表示参数错误,1 表示运行期错误(例如回放失败)。
func Run(args []string) {
	if len(args) < 2 {
		usage()
		os.Exit(2)
	}
	switch args[1] {
	case "record":
		recordCmd(args[2:])
	case "replay":
		replayCmd(args[2:])
	case "list", "ls":
		listCmd(args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", args[1])
		usage()
		os.Exit(2)
	}
}

// usage 输出顶级 help 文本到 stderr。
func usage() {
	fmt.Fprintln(os.Stderr, `webflow - 录制/回放 Jenkins 等 Web 操作的 POST 请求

用法:
  webflow record [-o flow.json] <url>          打开浏览器录制 POST 请求
  webflow replay [选项] [flow.json]            回放录制的请求 (默认: flow.json)
  webflow list   [-v]   [flow.json]            列出录制的请求 (默认: flow.json)

record 选项:
  -o   <file>   输出文件 (默认 flow.json)
  -all          录制所有写请求 (POST/PUT/DELETE/PATCH)。默认仅 POST
  -host <list>  仅录制指定 host (含子域)。默认是目标 URL 的 host。
                填 "*" 录制全部域名 (会包含 SSO 登录、第三方上报等噪音)

replay 选项:
  -f   <substr> 仅回放 URL 包含该子串的请求
  -i   <索引>   仅回放指定索引 (例如 -i 0,2,5 或 -i 3-7)
  -d   <时长>   每个请求之间的延时 (例如 500ms)
  -crumb=false  不自动刷新 Jenkins crumb
  -dry          只打印将要发送的请求,不真实发送
  -insecure     跳过 TLS 证书校验 (用于自签证书的 Jenkins)

示例:
  webflow record https://jenkins.example.com/
  webflow list flow.json
  webflow replay -f "/build" flow.json
  webflow replay -i 3 flow.json`)
}
