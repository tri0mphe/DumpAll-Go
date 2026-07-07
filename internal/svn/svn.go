package svn

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"dumpall-go/internal/dumper"
	"dumpall-go/pkg/utils"

	"github.com/fatih/color"
)

// SvnDumper 实现 .svn 源代码下载
type SvnDumper struct {
	dumper.BaseDumper
}

// NewSvnDumper 创建 SvnDumper 实例
func NewSvnDumper() *SvnDumper {
	return &SvnDumper{
		BaseDumper: dumper.BaseDumper{
			Name:        "svn",
			Description: "下载 .svn 源代码",
		},
	}
}

// Check 检查目标是否存在 .svn 信息泄露
func (d *SvnDumper) Check(targetURL string, client *http.Client) (bool, error) {
	// 确保URL以/结尾
	if !strings.HasSuffix(targetURL, "/") {
		targetURL += "/"
	}

	// 检查 .svn/entries 文件
	entriesURL := targetURL + ".svn/entries"
	resp, err := client.Head(entriesURL)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	// 检查 .svn/wc.db 文件 (SVN 1.7+)
	wcdbURL := targetURL + ".svn/wc.db"
	resp, err = client.Head(wcdbURL)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	return false, nil
}

// Execute 执行下载操作
func (d *SvnDumper) Execute(targetURL string, outdir string, proxyAddr string, force bool, debug bool, workers int, progressCb dumper.ProgressCallback) error {
	// 创建HTTP客户端（支持 http/https/socks5 代理）
	color.Cyan("[SVN] 初始化扫描: %s", targetURL)
	client, err := utils.CreateHTTPClient(proxyAddr)
	if err != nil {
		return fmt.Errorf("[SVN] 创建HTTP客户端失败: %v", err)
	}

	// 确保URL以/结尾
	if !strings.HasSuffix(targetURL, "/") {
		targetURL += "/"
	}

	// 创建输出目录
	if err := os.MkdirAll(outdir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %v", err)
	}

	// 定义常见的SVN文件
	svnFiles := []string{
		".svn/entries",
		".svn/wc.db",
		".svn/format",
		".svn/all-wcprops",
		".svn/props",
		".svn/text-base",
		".svn/tmp",
	}

	color.Cyan("[SVN] 共需检测 %d 个文件", len(svnFiles))
	// 下载文件
	for _, file := range svnFiles {
		fileURL := targetURL + file
		localPath := filepath.Join(outdir, file)

		// 创建目录
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			color.Red("[SVN] 创建目录失败 %s: %v", filepath.Dir(localPath), err)
			if progressCb != nil {
				progressCb(fileURL, 0, "创建目录失败")
			}
			continue
		}

		// 下载文件
		color.Yellow("[SVN] 请求: %s", fileURL)
		resp, err := client.Get(fileURL)
		if err != nil {
			color.Red("[SVN] 请求失败: %s -> %v", fileURL, err)
			if progressCb != nil {
				progressCb(fileURL, 0, "下载失败")
			}
			continue
		}

		// 调用进度回调
		if progressCb != nil {
			progressCb(fileURL, resp.StatusCode, localPath)
		}

		if resp.StatusCode != http.StatusOK {
			color.Yellow("[SVN] 跳过(状态码 %d): %s", resp.StatusCode, fileURL)
			resp.Body.Close()
			continue
		}

		// 创建本地文件
		f, err := os.Create(localPath)
		if err != nil {
			color.Red("[SVN] 创建本地文件失败 %s: %v", localPath, err)
			resp.Body.Close()
			continue
		}

		// 写入文件内容
		_, err = io.Copy(f, resp.Body)
		resp.Body.Close()
		f.Close()

		if err != nil {
			color.Red("[SVN] 写入失败 %s: %v", localPath, err)
			if progressCb != nil {
				progressCb(fileURL, 0, "写入失败")
			}
			continue
		}
		color.Green("[SVN] 已保存: %s -> %s", fileURL, localPath)
	}

	return nil
}

// Validate 验证URL是否有效
func (d *SvnDumper) Validate(url string) error {
	if !strings.HasSuffix(url, ".svn") && !strings.HasSuffix(url, ".svn/") {
		return fmt.Errorf("URL必须以.svn结尾")
	}
	return nil
}
