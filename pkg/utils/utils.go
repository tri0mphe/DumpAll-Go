package utils

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/net/proxy"
)

type Task struct {
	URL    string
	Outdir string
	Proxy  string
	Type   string
}

type Result struct {
	URL     string
	Error   error
	Output  string
	Start   time.Time
	End     time.Time
	Success bool
}

type Logger struct {
	debug bool
}

func NewLogger(debug bool) *Logger {
	return &Logger{debug: debug}
}

func (l *Logger) Info(format string, args ...interface{}) {
	color.Cyan(format, args...)
}

func (l *Logger) Success(format string, args ...interface{}) {
	color.Green(format, args...)
}

func (l *Logger) Error(format string, args ...interface{}) {
	color.Red(format, args...)
}

func (l *Logger) Debug(format string, args ...interface{}) {
	if l.debug {
		color.Yellow(format, args...)
	}
}

func ReadURLsFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %v", err)
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		url := strings.TrimSpace(scanner.Text())
		if url != "" {
			urls = append(urls, url)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取文件失败: %v", err)
	}

	fmt.Printf("总共读取到 %d 个URL\n", len(urls))
	return urls, nil
}

func ProcessTasks(tasks []Task, workerFunc func(Task) Result, workers int, logger *Logger) []Result {

	taskChan := make(chan Task, len(tasks))
	resultChan := make(chan Result, len(tasks))

	bar := progressbar.NewOptions(len(tasks),
		progressbar.OptionSetDescription("处理进度"),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskChan {
				result := workerFunc(task)
				resultChan <- result
				bar.Add(1)
			}
		}()
	}

	for _, task := range tasks {
		taskChan <- task
	}
	close(taskChan)

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var results []Result
	for result := range resultChan {
		results = append(results, result)
	}

	success := 0
	failed := 0
	var totalTime time.Duration
	for _, result := range results {
		if result.Success {
			success++
		} else {
			failed++
		}
		totalTime += result.End.Sub(result.Start)
	}

	logger.Info("\n处理完成: 成功 %d, 失败 %d, 总耗时: %v", success, failed, totalTime)

	return results
}

func GetHostFromURL(url string) string {
	url = strings.TrimPrefix(strings.TrimPrefix(url, "http://"), "https://")
	host := strings.Split(url, "/")[0]
	return host
}

func CreateOutputDir(outdir string) error {
	if err := os.MkdirAll(outdir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %v", err)
	}
	return nil
}

func ValidateURL(url string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("URL必须以http://或https://开头")
	}
	return nil
}

func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func GetHostname(targetURL string) string {
	u, err := url.Parse(targetURL)
	if err != nil {
		return "unknown"
	}
	hostname := u.Hostname()
	if hostname == "" {
		return "unknown"
	}
	return hostname
}

// CreateHTTPClient 创建带有代理支持的 HTTP 客户端
// 支持协议: http://, https://, socks5://, socks5h://
// proxyAddr 示例:
//
//	http://127.0.0.1:8080
//	http://user:pass@127.0.0.1:8080
//	socks5://127.0.0.1:1080
//	socks5://user:pass@127.0.0.1:1080
//	socks5h://127.0.0.1:1080   (socks5h = 远端DNS解析)
func CreateHTTPClient(proxyAddr string) (*http.Client, error) {
	if proxyAddr == "" {
		color.Cyan("[代理] 未设置代理，使用直连模式")
		return &http.Client{}, nil
	}

	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("[代理] 解析代理地址失败 %q: %v", proxyAddr, err)
	}

	scheme := strings.ToLower(proxyURL.Scheme)

	switch scheme {
	case "http", "https":
		// HTTP/HTTPS 代理
		transport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		client := &http.Client{Transport: transport}

		host := proxyURL.Hostname()
		port := proxyURL.Port()
		user := ""
		if proxyURL.User != nil {
			user = proxyURL.User.Username()
		}
		if user != "" {
			color.Cyan("[代理] 使用 %s 代理: %s:%s (用户: %s)", strings.ToUpper(scheme), host, port, user)
		} else {
			color.Cyan("[代理] 使用 %s 代理: %s:%s", strings.ToUpper(scheme), host, port)
		}
		return client, nil

	case "socks5", "socks5h":
		// SOCKS5 代理
		var auth *proxy.Auth
		if proxyURL.User != nil {
			pwd, _ := proxyURL.User.Password()
			auth = &proxy.Auth{
				User:     proxyURL.User.Username(),
				Password: pwd,
			}
		}

		host := proxyURL.Hostname()
		port := proxyURL.Port()
		if port == "" {
			port = "1080"
		}
		proxyHostPort := net.JoinHostPort(host, port)

		// socks5h 表示让代理服务器做DNS解析（远端DNS）
		dialer, err := proxy.SOCKS5("tcp", proxyHostPort, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("[代理] 创建 SOCKS5 代理失败 %q: %v", proxyAddr, err)
		}

		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}
		client := &http.Client{Transport: transport}

		if auth != nil {
			color.Cyan("[代理] 使用 SOCKS5 代理: %s (用户: %s, DNS解析: %s)",
				proxyHostPort, auth.User, map[bool]string{true: "远端(socks5h)", false: "本地(socks5)"}[scheme == "socks5h"])
		} else {
			color.Cyan("[代理] 使用 SOCKS5 代理: %s (无认证, DNS解析: %s)",
				proxyHostPort, map[bool]string{true: "远端(socks5h)", false: "本地(socks5)"}[scheme == "socks5h"])
		}
		return client, nil

	default:
		return nil, fmt.Errorf("[代理] 不支持的代理协议 %q，支持: http, https, socks5, socks5h", scheme)
	}
}
