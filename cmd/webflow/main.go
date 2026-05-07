// Command webflow 启动 Chrome 录制用户在 Jenkins 等站点上的 POST 操作,
// 并支持后续按需重放,适用于"手动操作一次,自动重复执行"的场景。
//
// 用法概览:
//
//	webflow record [-o flow.json] [-host h1,h2] <url>   打开浏览器录制
//	webflow list  [-v] <flow.json>                       列出录制内容
//	webflow replay [选项] <flow.json>                    回放录制的请求
//
// 详细参数请运行 `webflow -h` 查看,或参见仓库 README。
package main

import (
	"os"

	"github.com/dxasu/webflow"
)

func main() {
	webflow.Run(os.Args)
}
