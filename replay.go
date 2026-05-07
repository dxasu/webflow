// 本文件实现 replay 子命令:
//   - 加载 flow.json,把 cookie 灌进 cookiejar
//   - 默认对每个 host 自动获取一次最新的 Jenkins CSRF crumb,注入到请求头
//   - 按原顺序发出 POST / PUT / DELETE,根据 -f / -i / -d 等选项做过滤与节奏控制
package webflow

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 这些 header 由 http 库自行管理,不应从录制中透传
var skipHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"connection":        true,
	"keep-alive":        true,
	"transfer-encoding": true,
	"upgrade":           true,
	"proxy-connection":  true,
	"cookie":            true, // cookie 由 cookie jar 接管
	// CDP 上报的伪 header(HTTP/2 风格),http.Request 自动处理
	":authority": true,
	":method":    true,
	":path":      true,
	":scheme":    true,
}

// replayCmd 实现 `webflow replay` 子命令。
//
// 主要步骤:
//  1. 加载录制结果,按 base64 -> string 还原 body
//  2. 把 cookie 灌进一个 [cookiejar.Jar]
//  3. 默认对每个出现过的 host 调用 /crumbIssuer/api/json 获取最新 crumb (Jenkins 强制)
//  4. 按原顺序发出请求,根据 -f / -i / -d / -dry 等做过滤与节奏控制
//  5. 出现 4xx/5xx 时打印响应片段,方便排错
func replayCmd(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	filter := fs.String("f", "", "仅回放 URL 包含此子串的请求")
	idxStr := fs.String("i", "", "仅回放指定索引,逗号分隔。例如 0,2,5")
	delay := fs.Duration("d", 0, "每次请求之间的延时")
	refreshCrumb := fs.Bool("crumb", true, "自动刷新 Jenkins crumb")
	dryRun := fs.Bool("dry", false, "只打印不真实发送")
	insecure := fs.Bool("insecure", false, "跳过 TLS 证书校验")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	path := flowFileFromArgs(fs.Args())
	flow, err := loadFlow(path)
	if err != nil {
		log.Fatalf("加载录制文件 %s 失败: %v", path, err)
	}
	fmt.Printf("文件: %s\n", path)

	indexes, err := parseIndexes(*idxStr)
	if err != nil {
		log.Fatalf("索引参数无效: %v", err)
	}

	jar, _ := cookiejar.New(nil)
	loadCookieJar(jar, flow)

	tr := &http.Transport{}
	if *insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{
		Jar:       jar,
		Transport: tr,
		Timeout:   60 * time.Second,
		// 不自动跟随重定向,看清楚每个请求的真实状态
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Jenkins 的 CSRF crumb 通常会过期。这里尝试为每个 host 拉取一个新的。
	crumbs := make(map[string]struct{ field, value string })
	if *refreshCrumb {
		for _, r := range flow.Requests {
			u, err := url.Parse(r.URL)
			if err != nil {
				continue
			}
			origin := u.Scheme + "://" + u.Host
			if _, ok := crumbs[origin]; ok {
				continue
			}
			f, v, err := fetchJenkinsCrumb(client, origin)
			if err == nil && f != "" {
				crumbs[origin] = struct{ field, value string }{f, v}
				fmt.Printf("[CRUMB] %s -> %s=%s\n", origin, f, shortenURL(v))
			}
		}
	}

	ok, fail := 0, 0
	for i, r := range flow.Requests {
		if !shouldRunIndex(i, indexes) {
			continue
		}
		if *filter != "" && !strings.Contains(r.URL, *filter) {
			continue
		}

		var body io.Reader
		if r.PostData != "" {
			body = bytes.NewReader([]byte(r.PostData))
		}
		req, err := http.NewRequest(r.Method, r.URL, body)
		if err != nil {
			fmt.Printf("[%3d] 构造请求失败: %v\n", i, err)
			fail++
			continue
		}
		for k, v := range r.Headers {
			if skipHeaders[strings.ToLower(k)] {
				continue
			}
			if strings.HasPrefix(k, ":") {
				continue
			}
			req.Header.Set(k, v)
		}

		// 注入新的 crumb
		if u, err := url.Parse(r.URL); err == nil {
			if c, ok := crumbs[u.Scheme+"://"+u.Host]; ok && c.field != "" {
				req.Header.Set(c.field, c.value)
			}
		}

		if *dryRun {
			fmt.Printf("[DRY %3d] %s %s\n", i, req.Method, shortenURL(r.URL))
			if r.PostData != "" {
				fmt.Printf("          body(%s): %s\n",
					fmtBytes(len(r.PostData)), shortenURL(r.PostData))
			}
			continue
		}

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("[%3d] %s %s -> 错误: %v\n", i, req.Method, shortenURL(r.URL), err)
			fail++
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		dur := time.Since(start)

		statusTag := "OK"
		if resp.StatusCode >= 400 {
			statusTag = "FAIL"
			fail++
		} else {
			ok++
		}
		fmt.Printf("[%3d %s] %d %s %s (%s, %s)\n",
			i, statusTag, resp.StatusCode, req.Method, shortenURL(r.URL),
			fmtBytes(len(respBody)), dur.Round(time.Millisecond))
		// 失败时把响应内容前几行打印出来,方便排查
		if resp.StatusCode >= 400 && len(respBody) > 0 {
			snippet := string(respBody)
			if len(snippet) > 300 {
				snippet = snippet[:300] + "..."
			}
			fmt.Printf("      响应: %s\n", strings.ReplaceAll(snippet, "\n", " "))
		}

		if *delay > 0 {
			time.Sleep(*delay)
		}
	}

	fmt.Printf("\n完成: 成功 %d, 失败 %d\n", ok, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

// loadCookieJar 把录制中保存的 [Cookie] 列表灌进 cookiejar。
// 同 host 的 cookie 会被一次性 Set,避免逐条调用引发的 jar 内部对比开销。
func loadCookieJar(jar *cookiejar.Jar, flow *Flow) {
	// 按 domain 分组并设置
	byOrigin := make(map[string][]*http.Cookie)
	for _, ck := range flow.Cookies {
		domain := strings.TrimPrefix(ck.Domain, ".")
		if domain == "" {
			continue
		}
		scheme := "https"
		if !ck.Secure {
			// 优先 https,失败时 http jar 也能取到
			scheme = "https"
		}
		key := scheme + "://" + domain
		c := &http.Cookie{
			Name:     ck.Name,
			Value:    ck.Value,
			Path:     ck.Path,
			Secure:   ck.Secure,
			HttpOnly: ck.HTTPOnly,
		}
		byOrigin[key] = append(byOrigin[key], c)
	}
	for origin, cks := range byOrigin {
		u, err := url.Parse(origin)
		if err != nil {
			continue
		}
		jar.SetCookies(u, cks)
	}
}

// fetchJenkinsCrumb 尝试通过 /crumbIssuer/api/json 获取新的 CSRF token。
// 不是 Jenkins 时会返回 ("", "", nil) 或错误,调用方忽略即可。
func fetchJenkinsCrumb(client *http.Client, origin string) (field, value string, err error) {
	resp, err := client.Get(origin + "/crumbIssuer/api/json")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var c struct {
		Crumb             string `json:"crumb"`
		CrumbRequestField string `json:"crumbRequestField"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return "", "", err
	}
	return c.CrumbRequestField, c.Crumb, nil
}

// parseIndexes 解析 -i 参数,例如 "0,2,5-7" -> {0,2,5,6,7}。
// 返回 nil 表示用户未指定过滤,所有索引都生效。
func parseIndexes(s string) (map[int]bool, error) {
	if s == "" {
		return nil, nil
	}
	out := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// 支持 a-b 范围
		if dash := strings.Index(part, "-"); dash > 0 {
			a, err1 := strconv.Atoi(part[:dash])
			b, err2 := strconv.Atoi(part[dash+1:])
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("无效范围: %s", part)
			}
			for i := a; i <= b; i++ {
				out[i] = true
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, nil
}

func shouldRunIndex(i int, indexes map[int]bool) bool {
	if indexes == nil {
		return true
	}
	return indexes[i]
}
