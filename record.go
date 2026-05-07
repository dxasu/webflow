// 本文件实现 record 子命令:
//   - 启动一个真实的 Chrome 窗口,让用户自行登录与点击操作
//   - 通过 CDP 的 Network.requestWillBeSent 事件实时抓取 POST 请求与 body
//   - 后台周期性地把 cookie 全量同步到内存,Ctrl+C 时再补一次,确保不丢登录态
package webflow

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// recordCmd 实现 `webflow record` 子命令。
//
// 子命令解析完参数后会:
//  1. 通过 [ensureBrowser] 确保有可用的 Chrome / Chromium
//  2. 启动一个非 headless 的 Chrome 实例并打开目标 URL
//  3. 通过 CDP 监听 [network.EventRequestWillBeSent],按 host / 静态资源等规则过滤
//  4. 起一个后台 goroutine 周期性同步 cookie ([runCookiePoller])
//  5. 等待用户 Ctrl+C 或浏览器关闭,然后把 [Flow] 写到磁盘
func recordCmd(args []string) {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	output := fs.String("o", "flow.json", "输出文件")
	includeAll := fs.Bool("all", false, "录制所有写请求 (POST/PUT/DELETE/PATCH)")
	headless := fs.Bool("headless", false, "无头模式 (一般用于调试)")
	hostSpec := fs.String("host", "",
		`仅录制指定 host 的请求,逗号分隔。`+
			`默认推断目标 URL 的 host(强烈建议),`+
			`填 "*" 录制全部域名。`)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: webflow record [-o flow.json] [-host host1,host2] <url>")
		os.Exit(2)
	}
	target := fs.Arg(0)

	browserPath, err := ensureBrowser()
	if err != nil {
		log.Fatalf("%v", err)
	}

	hostMatch := buildHostMatcher(*hostSpec, target)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browserPath),
		chromedp.Flag("headless", *headless),
		chromedp.Flag("disable-gpu", false),
		chromedp.Flag("hide-scrollbars", false),
		chromedp.Flag("mute-audio", false),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	flow := &Flow{
		BaseURL:   target,
		StartedAt: time.Now(),
		Requests:  []*RecordedRequest{},
	}
	var mu sync.Mutex

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		e, ok := ev.(*network.EventRequestWillBeSent)
		if !ok {
			return
		}
		req := e.Request
		if !shouldRecord(req.Method, req.URL, *includeAll) {
			return
		}
		if !hostMatch(req.URL) {
			return
		}
		rec := &RecordedRequest{
			Time:        time.Now(),
			Method:      req.Method,
			URL:         req.URL,
			Headers:     convertHeaders(req.Headers),
			HasPostData: req.HasPostData,
		}
		// 优先用事件里携带的 PostDataEntries (base64 编码),
		// 这是同步可得的、最可靠的请求体来源。
		if rec.HasPostData {
			rec.PostData = decodePostDataEntries(req.PostDataEntries)
		}

		mu.Lock()
		flow.Requests = append(flow.Requests, rec)
		idx := len(flow.Requests) - 1
		mu.Unlock()

		fmt.Printf("[REC %3d] %s %s\n", idx, req.Method, shortenURL(req.URL))

		// 极少数情况下 PostDataEntries 为空,再异步兜底
		if rec.HasPostData && rec.PostData == "" {
			go fetchPostData(ctx, e.RequestID, rec, &mu)
		}
	})

	if err := chromedp.Run(ctx,
		network.Enable(),
		chromedp.Navigate(target),
	); err != nil {
		log.Fatalf("启动浏览器失败: %v", err)
	}

	fmt.Println()
	fmt.Println("======================================================")
	fmt.Println(" 浏览器已打开,请自行登录并执行需要录制的操作。")
	fmt.Println(" 完成后回到终端按 Ctrl+C 保存录制结果。")
	fmt.Println(" (浏览器窗口可以保持打开,Ctrl+C 时再统一关闭)")
	fmt.Println("======================================================")
	fmt.Println()

	// 后台周期 dump cookie。这样即便浏览器先于 Ctrl+C 被关闭,
	// 我们也能拿到关闭前最近一次的快照。
	cookieStop := make(chan struct{})
	cookieDone := make(chan struct{})
	go runCookiePoller(ctx, &mu, flow, cookieStop, cookieDone)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		fmt.Println("\n收到中断信号,正在保存...")
	case <-ctx.Done():
		fmt.Println("\n浏览器已关闭,正在保存...")
	}

	close(cookieStop)
	<-cookieDone

	// 让最后一批 fetchPostData 兜底有机会完成
	time.Sleep(500 * time.Millisecond)

	// 浏览器还活着的话,再抓一次最新 cookie
	if ctx.Err() == nil {
		_ = dumpCookies(ctx, &mu, flow)
	} else if len(flow.Cookies) == 0 {
		log.Printf("警告: 浏览器上下文已结束且未能采集到 cookie。请重录,或在 Ctrl+C 之前不要主动关闭浏览器窗口。")
	}

	mu.Lock()
	flow.EndedAt = time.Now()
	for _, r := range flow.Requests {
		if ua := r.Headers["User-Agent"]; ua != "" {
			flow.UserAgent = ua
			break
		}
	}
	count := len(flow.Requests)
	mu.Unlock()

	if err := saveFlow(*output, flow); err != nil {
		log.Fatalf("保存失败: %v", err)
	}
	fmt.Printf("已录制 %d 个请求,%d 个 cookie,已保存到 %s\n",
		count, len(flow.Cookies), *output)
}

// runCookiePoller 周期性把当前浏览器 cookie 全量同步到 flow.Cookies。
func runCookiePoller(ctx context.Context, mu *sync.Mutex, flow *Flow,
	stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	// 启动后立即抓一次,登录完成立刻有备份
	_ = dumpCookies(ctx, mu, flow)
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			_ = dumpCookies(ctx, mu, flow)
		}
	}
}

// dumpCookies 调用一次 Network.getCookies,把结果"全量替换式"写入 flow.Cookies。
// 用全量替换是为了反映 cookie 的最新状态(例如登录后旧 cookie 被新 cookie 覆盖)。
func dumpCookies(parent context.Context, mu *sync.Mutex, flow *Flow) error {
	cctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var cks []*network.Cookie
	err := chromedp.Run(cctx, chromedp.ActionFunc(func(c context.Context) error {
		var err error
		cks, err = network.GetCookies().Do(c)
		return err
	}))
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	flow.Cookies = flow.Cookies[:0]
	for _, ck := range cks {
		flow.Cookies = append(flow.Cookies, Cookie{
			Name:     ck.Name,
			Value:    ck.Value,
			Domain:   ck.Domain,
			Path:     ck.Path,
			Secure:   ck.Secure,
			HTTPOnly: ck.HTTPOnly,
			Expires:  ck.Expires,
		})
	}
	return nil
}

// decodePostDataEntries 把 CDP 事件里的 base64 分块还原成原始 body。
func decodePostDataEntries(entries []*network.PostDataEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, entry := range entries {
		if entry == nil || entry.Bytes == "" {
			continue
		}
		// CDP 协议约定 bytes 字段为 base64
		if decoded, err := base64.StdEncoding.DecodeString(entry.Bytes); err == nil {
			sb.Write(decoded)
		} else {
			// 极少数实现直接给的就是原文,兜底
			sb.WriteString(entry.Bytes)
		}
	}
	return sb.String()
}

// fetchPostData 当事件里没有 PostDataEntries 时的兜底:主动调用 CDP 取回。
func fetchPostData(parent context.Context, id network.RequestID, rec *RecordedRequest, mu *sync.Mutex) {
	cctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	_ = chromedp.Run(cctx, chromedp.ActionFunc(func(c context.Context) error {
		data, err := network.GetRequestPostData(id).Do(c)
		if err != nil {
			return nil
		}
		mu.Lock()
		rec.PostData = string(data)
		mu.Unlock()
		return nil
	}))
}

// convertHeaders 把 chromedp 的 [network.Headers] (interface{} 值) 拍平成 string map,
// 便于直接序列化到 JSON 与回放时还原到 http.Header。
func convertHeaders(in network.Headers) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch s := v.(type) {
		case string:
			out[k] = s
		default:
			out[k] = fmt.Sprintf("%v", s)
		}
	}
	return out
}

// buildHostMatcher 根据 -host 选项与 baseURL 生成匹配函数。
//   - spec == ""    -> 仅匹配 baseURL 的 host (含子域)
//   - spec == "*"   -> 全部放行
//   - spec == "a,b" -> 匹配列表中的任意 host (含子域)
func buildHostMatcher(spec, baseURL string) func(string) bool {
	if spec == "*" {
		return func(string) bool { return true }
	}
	var hosts []string
	if spec == "" {
		if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
			hosts = []string{u.Hostname()}
		}
	} else {
		for _, h := range strings.Split(spec, ",") {
			h = strings.TrimSpace(h)
			if h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	// 没有可匹配的 host(异常情况),则全部放行,避免空录制
	if len(hosts) == 0 {
		return func(string) bool { return true }
	}
	return func(rawURL string) bool {
		u, err := url.Parse(rawURL)
		if err != nil {
			return false
		}
		host := u.Hostname()
		for _, h := range hosts {
			if host == h || strings.HasSuffix(host, "."+h) {
				return true
			}
		}
		return false
	}
}
