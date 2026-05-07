// 本文件定义录制结果的持久化结构 ([Flow] / [RecordedRequest] / [Cookie])
// 以及录制阶段使用的请求过滤规则。
package webflow

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// RecordedRequest 表示一次抓取到的 HTTP 请求。
// 字段集合既要供 JSON 持久化使用,也要在录制 / 回放时直接读写。
type RecordedRequest struct {
	Time        time.Time         `json:"time"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers,omitempty"`
	HasPostData bool              `json:"has_post_data"`
	PostData    string            `json:"post_data,omitempty"`
	// 备注信息,用户可手动编辑 flow.json 时使用
	Note string `json:"note,omitempty"`
}

// Cookie 是录制结束时浏览器持有的 cookie 快照。
// 字段裁剪自 chromedp/cdproto network.Cookie,只保留回放真正需要的部分。
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

// Flow 表示一次完整的录制结果,以 JSON 形式持久化到磁盘。
type Flow struct {
	BaseURL   string             `json:"base_url"`
	StartedAt time.Time          `json:"started_at"`
	EndedAt   time.Time          `json:"ended_at"`
	UserAgent string             `json:"user_agent,omitempty"`
	Cookies   []Cookie           `json:"cookies,omitempty"`
	Requests  []*RecordedRequest `json:"requests"`
}

// defaultFlowFile 是录制结果的默认文件名。
// list / replay 在没有传文件参数时会回退到它。
const defaultFlowFile = "flow.json"

// flowFileFromArgs 从子命令的位置参数里取 flow 文件路径,
// 缺省时回退到 [defaultFlowFile]。
func flowFileFromArgs(args []string) string {
	if len(args) >= 1 && args[0] != "" {
		return args[0]
	}
	return defaultFlowFile
}

// saveFlow 把 [Flow] 以缩进过的 JSON 写到 path,文件已存在时会被覆盖。
func saveFlow(path string, f *Flow) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

// loadFlow 读取并反序列化 saveFlow 写出的 JSON 文件。
func loadFlow(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Flow
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// shouldRecord 判断一个请求是否值得被记录:
//   - 必须是写请求 (默认仅 POST,includeAllWrite 为 true 时也含 PUT/DELETE/PATCH)
//   - 必须是 http(s) 协议
//   - 排除常见的静态资源、Jenkins 自身的轮询接口、第三方上报域名
func shouldRecord(method, rawURL string, includeAllWrite bool) bool {
	method = strings.ToUpper(method)
	if includeAllWrite {
		switch method {
		case "POST", "PUT", "DELETE", "PATCH":
		default:
			return false
		}
	} else if method != "POST" {
		return false
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !strings.HasPrefix(u.Scheme, "http") {
		return false
	}

	// 过滤静态资源
	lower := strings.ToLower(u.Path)
	for _, ext := range staticExts {
		if strings.HasSuffix(lower, ext) {
			return false
		}
	}

	// 过滤 Jenkins 噪音端点(轮询日志、tree 查询等)
	for _, kw := range noisyKeywords {
		if strings.Contains(u.Path, kw) {
			return false
		}
	}
	// 过滤一些常见的浏览器/CDN 上报
	for _, host := range skipHosts {
		if strings.HasSuffix(u.Host, host) {
			return false
		}
	}
	return true
}

var (
	staticExts = []string{
		".js", ".css", ".png", ".jpg", ".jpeg", ".gif",
		".svg", ".ico", ".woff", ".woff2", ".ttf", ".eot",
		".map", ".webp", ".mp4",
	}
	noisyKeywords = []string{
		"/progressiveLog",
		"/progressiveHtml",
		"/log/tail",
		"/logText/progressiveText",
		"/console",
	}
	skipHosts = []string{
		"google-analytics.com",
		"googletagmanager.com",
		"doubleclick.net",
		"sentry.io",
	}
)

// shortenURL 用于在终端日志中截断过长的 URL,避免单行刷屏。
func shortenURL(s string) string {
	const max = 100
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// fmtBytes 把字节数格式化成 B / KB / MB 这种人类可读形式。
func fmtBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/1024/1024)
	}
}
