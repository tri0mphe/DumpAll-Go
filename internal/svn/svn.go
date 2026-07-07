package svn

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"dumpall-go/internal/dumper"
	"dumpall-go/pkg/utils"

	"github.com/fatih/color"
	_ "modernc.org/sqlite"
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

// --------------------------------------------------------------------------
// SVN Extract：从 .svn 信息泄露中还原完整源代码
// --------------------------------------------------------------------------

// SvnFileEntry 表示从 SVN 元数据中解析出的一条文件记录
type SvnFileEntry struct {
	RelPath string // 相对于项目根的路径，例如 "src/main.go"
	Kind    string // "file" 或 "dir"
}

// ParseEntriesFromData 解析 SVN 1.6 及更早的 entries 文件（纯文本格式）
// entries 格式：每条记录由 12 行组成，第1行是文件名，第2行是类型(file/dir)
func ParseEntriesFromData(data []byte) []SvnFileEntry {
	var entries []SvnFileEntry
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	// 第一行是格式版本号（如 "10"），跳过
	if !scanner.Scan() {
		return entries
	}

	// 每条记录以空行分隔，字段依次为：
	// [0] 名称, [1] kind(file/dir), [2] revision, [3] url, ...
	var record []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// 空行表示一条记录结束
			if len(record) >= 2 {
				name := record[0]
				kind := record[1]
				if name != "" && (kind == "file" || kind == "dir") {
					entries = append(entries, SvnFileEntry{RelPath: name, Kind: kind})
				}
			}
			record = nil
			continue
		}
		record = append(record, line)
	}
	// 处理最后一条记录（文件末尾无空行时）
	if len(record) >= 2 {
		name := record[0]
		kind := record[1]
		if name != "" && (kind == "file" || kind == "dir") {
			entries = append(entries, SvnFileEntry{RelPath: name, Kind: kind})
		}
	}
	return entries
}

// ParseWcDbFromFile 解析 SVN 1.7+ 的 wc.db（SQLite）文件，返回被追踪的文件列表
func ParseWcDbFromFile(dbPath string) ([]SvnFileEntry, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 wc.db 失败: %v", err)
	}
	defer db.Close()

	// NODES 表中 local_relpath 为空字符串代表根节点，kind=1 表示 file，kind=2 表示 dir
	rows, err := db.Query(`
		SELECT local_relpath, kind
		FROM NODES
		WHERE local_relpath != ''
		ORDER BY local_relpath
	`)
	if err != nil {
		return nil, fmt.Errorf("查询 wc.db NODES 表失败: %v", err)
	}
	defer rows.Close()

	var entries []SvnFileEntry
	for rows.Next() {
		var relPath string
		var kind int
		if err := rows.Scan(&relPath, &kind); err != nil {
			continue
		}
		kindStr := "file"
		if kind == 2 {
			kindStr = "dir"
		}
		entries = append(entries, SvnFileEntry{RelPath: relPath, Kind: kindStr})
	}
	return entries, rows.Err()
}

// downloadFile 下载单个远程文件到本地路径
func downloadFile(client *http.Client, fileURL, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("创建目录失败: %v", err)
	}

	resp, err := client.Get(fileURL)
	if err != nil {
		return fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %v", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("写入失败: %v", err)
	}
	return nil
}

// Extract 从目标 URL 的 .svn 信息泄露中还原完整源代码
//
// 流程：
//  1. 尝试下载 .svn/entries（SVN 1.6-）或 .svn/wc.db（SVN 1.7+）
//  2. 解析出被版本控制的文件列表
//  3. 对每个文件，按以下顺序尝试获取内容：
//     a. .svn/text-base/<filename>.svn-base（SVN 1.6- 存放原始文件副本）
//     b. 直接从根路径下载（部分服务器目录遍历可直接访问）
//  4. 还原为真实的源码目录结构输出到 outdir
func (d *SvnDumper) Extract(targetURL string, outdir string, proxyAddr string, workers int, progressCb dumper.ProgressCallback) error {
	color.Cyan("[SVN-Extract] 开始提取源代码: %s", targetURL)

	client, err := utils.CreateHTTPClient(proxyAddr)
	if err != nil {
		return fmt.Errorf("[SVN-Extract] 创建HTTP客户端失败: %v", err)
	}

	if !strings.HasSuffix(targetURL, "/") {
		targetURL += "/"
	}

	svnBase := targetURL + ".svn/"
	extractDir := filepath.Join(outdir, "extracted")
	svnDir := filepath.Join(outdir, ".svn")

	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %v", err)
	}
	if err := os.MkdirAll(svnDir, 0755); err != nil {
		return fmt.Errorf("创建 .svn 缓存目录失败: %v", err)
	}

	// ── 阶段1：尝试解析 entries（SVN 1.6-）──────────────────────────────
	var fileEntries []SvnFileEntry
	svnFormat := ""

	entriesURL := svnBase + "entries"
	color.Yellow("[SVN-Extract] 尝试下载 entries: %s", entriesURL)
	entriesLocal := filepath.Join(svnDir, "entries")
	if err := downloadFile(client, entriesURL, entriesLocal); err == nil {
		data, readErr := os.ReadFile(entriesLocal)
		if readErr == nil && len(data) > 0 {
			fileEntries = ParseEntriesFromData(data)
			svnFormat = "entries(SVN 1.6-)"
			color.Green("[SVN-Extract] entries 解析成功，发现 %d 条记录", len(fileEntries))
		}
	} else {
		color.Yellow("[SVN-Extract] entries 不可用: %v", err)
	}

	// ── 阶段2：尝试解析 wc.db（SVN 1.7+）────────────────────────────────
	if len(fileEntries) == 0 {
		wcdbURL := svnBase + "wc.db"
		color.Yellow("[SVN-Extract] 尝试下载 wc.db: %s", wcdbURL)
		wcdbLocal := filepath.Join(svnDir, "wc.db")
		if err := downloadFile(client, wcdbURL, wcdbLocal); err == nil {
			parsed, parseErr := ParseWcDbFromFile(wcdbLocal)
			if parseErr != nil {
				color.Red("[SVN-Extract] wc.db 解析失败: %v", parseErr)
			} else {
				fileEntries = parsed
				svnFormat = "wc.db(SVN 1.7+)"
				color.Green("[SVN-Extract] wc.db 解析成功，发现 %d 条记录", len(fileEntries))
			}
		} else {
			color.Yellow("[SVN-Extract] wc.db 不可用: %v", err)
		}
	}

	if len(fileEntries) == 0 {
		return fmt.Errorf("[SVN-Extract] 无法获取文件列表，请先确认目标存在 .svn/entries 或 .svn/wc.db 泄露")
	}

	color.Cyan("[SVN-Extract] 使用格式: %s，共 %d 个条目，开始提取文件...", svnFormat, len(fileEntries))

	// ── 阶段3：逐个文件下载还原 ──────────────────────────────────────────
	successCount := 0
	failCount := 0

	for _, entry := range fileEntries {
		if entry.Kind == "dir" {
			// 目录只需创建，不用下载
			dirPath := filepath.Join(extractDir, filepath.FromSlash(entry.RelPath))
			if err := os.MkdirAll(dirPath, 0755); err != nil {
				color.Red("[SVN-Extract] 创建目录失败 %s: %v", entry.RelPath, err)
			} else {
				color.Cyan("[SVN-Extract] 创建目录: %s", entry.RelPath)
			}
			continue
		}

		localPath := filepath.Join(extractDir, filepath.FromSlash(entry.RelPath))

		// 策略1：从 text-base 下载（SVN 1.6- 存储原始副本）
		// 路径形如 .svn/text-base/filename.svn-base
		// 对于子目录中的文件，SVN 1.6 在各子目录的 .svn/text-base/ 下分别存放
		baseName := filepath.Base(entry.RelPath)
		dirPart := filepath.Dir(entry.RelPath)
		var textBaseURL string
		if dirPart == "." || dirPart == "" {
			textBaseURL = svnBase + "text-base/" + baseName + ".svn-base"
		} else {
			textBaseURL = targetURL + filepath.ToSlash(dirPart) + "/.svn/text-base/" + baseName + ".svn-base"
		}

		color.Yellow("[SVN-Extract] 尝试 text-base: %s", textBaseURL)
		err := downloadFile(client, textBaseURL, localPath)
		if err == nil {
			color.Green("[SVN-Extract] ✓ text-base: %s", entry.RelPath)
			if progressCb != nil {
				progressCb(textBaseURL, http.StatusOK, localPath)
			}
			successCount++
			continue
		}

		// 策略2：直接从根路径下载原文件（目录遍历或直接访问）
		directURL := targetURL + entry.RelPath
		color.Yellow("[SVN-Extract] 尝试直接下载: %s", directURL)
		err = downloadFile(client, directURL, localPath)
		if err == nil {
			color.Green("[SVN-Extract] ✓ 直接下载: %s", entry.RelPath)
			if progressCb != nil {
				progressCb(directURL, http.StatusOK, localPath)
			}
			successCount++
			continue
		}

		color.Red("[SVN-Extract] ✗ 无法获取: %s (text-base 和直接下载均失败)", entry.RelPath)
		if progressCb != nil {
			progressCb(directURL, 0, "提取失败")
		}
		failCount++
	}

	color.Green("[SVN-Extract] 提取完成！成功: %d，失败: %d，输出目录: %s", successCount, failCount, extractDir)
	return nil
}
