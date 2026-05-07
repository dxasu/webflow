// 本文件实现 list 子命令,以可读形式打印录制内容,
// 方便在执行 replay 之前确认要回放哪些请求。
package webflow

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

// listCmd 实现 `webflow list` 子命令,把 [Flow] 渲染成可读文本。
//   - 不带参数: 仅打印每个请求的 method + URL + body 大小
//   - -v:        额外打印请求头与 body 内容(过长会截断)
//
// 文件参数省略时默认使用 [defaultFlowFile]。
func listCmd(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	verbose := fs.Bool("v", false, "显示请求 body 和 headers")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	path := flowFileFromArgs(fs.Args())
	flow, err := loadFlow(path)
	if err != nil {
		log.Fatalf("加载录制文件 %s 失败: %v", path, err)
	}
	fmt.Printf("文件: %s\n", path)

	fmt.Printf("基址: %s\n", flow.BaseURL)
	fmt.Printf("开始: %s\n", flow.StartedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("结束: %s\n", flow.EndedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("Cookie 数: %d\n", len(flow.Cookies))
	fmt.Printf("请求数: %d\n", len(flow.Requests))
	fmt.Println(strings.Repeat("-", 60))

	for i, r := range flow.Requests {
		fmt.Printf("[%3d] %s %s\n", i, r.Method, r.URL)
		if r.HasPostData {
			fmt.Printf("      body: %s\n", fmtBytes(len(r.PostData)))
		}
		if *verbose {
			for k, v := range r.Headers {
				fmt.Printf("        %s: %s\n", k, v)
			}
			if r.PostData != "" {
				preview := r.PostData
				if len(preview) > 500 {
					preview = preview[:500] + "..."
				}
				fmt.Printf("      ----- body -----\n%s\n", preview)
			}
		}
	}
}
