// 本文件实现 Chrome / Chromium 浏览器的检测与一键安装。
// 录制依赖一个支持 CDP 的 Chromium 内核浏览器,缺失时会引导用户通过
// 系统包管理器 (macOS 的 brew、Linux 的 apt/dnf/yum/pacman) 自动安装。
package webflow

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// detectBrowser 返回检测到的 Chrome/Chromium 可执行文件路径,
// 没找到时返回空字符串。
func detectBrowser() string {
	for _, p := range browserCandidates() {
		if fileExists(p) {
			return p
		}
	}
	for _, name := range []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"chrome",
	} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func browserCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			os.ExpandEnv("$HOME/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
		}
	case "linux":
		return []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			"/opt/google/chrome/chrome",
		}
	case "windows":
		return []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			os.ExpandEnv(`${LOCALAPPDATA}\Google\Chrome\Application\chrome.exe`),
		}
	}
	return nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// ensureBrowser 在缺失浏览器时询问用户并尝试自动安装,
// 返回最终可用的可执行文件路径。
func ensureBrowser() (string, error) {
	if p := detectBrowser(); p != "" {
		return p, nil
	}

	fmt.Fprintln(os.Stderr, "未检测到 Chrome/Chromium 浏览器,无法启动录制。")
	if !askYesNo("是否现在自动安装 Google Chrome?") {
		return "", fmt.Errorf("已取消安装。请手动安装 Chrome 后重试")
	}

	if err := installChrome(); err != nil {
		return "", err
	}

	p := detectBrowser()
	if p == "" {
		return "", fmt.Errorf("安装似乎已完成,但仍未找到 Chrome 可执行文件,请检查安装结果")
	}
	fmt.Fprintf(os.Stderr, "Chrome 已就绪: %s\n", p)
	return p, nil
}

// askYesNo 在 stdin 是 tty 的前提下交互式询问 y/N。
// 非交互场景 (脚本管道、CI) 直接返回 false,避免卡住进程。
func askYesNo(prompt string) bool {
	// 非交互终端时不再追问,避免脚本场景下卡住
	fi, _ := os.Stdin.Stat()
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		fmt.Fprintln(os.Stderr, "(stdin 不是终端,默认拒绝)")
		return false
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// installChrome 按当前 OS 选择合适的安装策略。
func installChrome() error {
	switch runtime.GOOS {
	case "darwin":
		return installChromeDarwin()
	case "linux":
		return installChromeLinux()
	default:
		return fmt.Errorf("当前平台 %s 暂不支持自动安装,请前往 https://www.google.com/chrome 手动安装", runtime.GOOS)
	}
}

// installChromeDarwin 优先使用 Homebrew Cask 安装 Google Chrome。
func installChromeDarwin() error {
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("未找到 Homebrew。请先安装 brew (https://brew.sh) 后重试,或手动下载 Chrome")
	}
	fmt.Fprintln(os.Stderr, "执行: brew install --cask google-chrome")
	return runForeground("brew", "install", "--cask", "google-chrome")
}

// installChromeLinux 根据可用包管理器选择策略。
func installChromeLinux() error {
	if _, err := exec.LookPath("apt-get"); err == nil {
		fmt.Fprintln(os.Stderr, "执行: sudo apt-get update && sudo apt-get install -y chromium-browser")
		if err := runForeground("sudo", "apt-get", "update"); err != nil {
			return err
		}
		// 优先 chromium-browser, 失败则尝试 chromium
		if err := runForeground("sudo", "apt-get", "install", "-y", "chromium-browser"); err == nil {
			return nil
		}
		return runForeground("sudo", "apt-get", "install", "-y", "chromium")
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		fmt.Fprintln(os.Stderr, "执行: sudo dnf install -y chromium")
		return runForeground("sudo", "dnf", "install", "-y", "chromium")
	}
	if _, err := exec.LookPath("yum"); err == nil {
		fmt.Fprintln(os.Stderr, "执行: sudo yum install -y chromium")
		return runForeground("sudo", "yum", "install", "-y", "chromium")
	}
	if _, err := exec.LookPath("pacman"); err == nil {
		fmt.Fprintln(os.Stderr, "执行: sudo pacman -S --noconfirm chromium")
		return runForeground("sudo", "pacman", "-S", "--noconfirm", "chromium")
	}
	return fmt.Errorf("未识别到受支持的包管理器(apt/dnf/yum/pacman),请手动安装 Chromium")
}

// runForeground 透传当前进程的 stdin/stdout/stderr 来运行外部命令,
// 以便用户能看到 brew / sudo 的进度条和密码提示。
func runForeground(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
