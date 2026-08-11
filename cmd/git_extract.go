package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"dumpall-go/internal/git"
	"dumpall-go/pkg/utils"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	gitExtractURL     string
	gitExtractOutdir  string
	gitExtractProxy   string
	gitExtractWorkers int
)

// gitExtractCmd 实现 git-extract 子命令
// 用于从已确认存在 .git 信息泄露的目标中提取完整源代码
var gitExtractCmd = &cobra.Command{
	Use:   "git-extract",
	Short: "从 .git 信息泄露中提取完整源代码",
	Long: `git-extract 专用于从已确认存在 .git 信息泄露的目标中还原完整源代码。
实现原理参照业界标准工具 GitHack (https://github.com/lijiejie/GitHack) 并做了增强：

  1. index 模式（主策略，等价于 GitHack 做法）
     下载并解析 .git/index（二进制格式），获取工作区所有被跟踪文件的
     (文件路径, blob sha1) 列表，再从 .git/objects/<sha1前2位>/<sha1后38位>
     逐个下载对象，zlib 解压后去掉 "blob <size>\0" 头，还原为原始文件内容。

  2. tree 递归模式（增强策略，弥补 index 缺失场景）
     当 .git/index 不可访问时，通过 .git/HEAD 找到当前分支引用，
     再从对应 ref 文件（或已打包的 .git/packed-refs）取得 commit sha1，
     下载 commit 对象解析出根 tree，递归遍历 tree/blob 对象还原完整目录结构。

两种模式的结果会自动合并去重，尽可能还原出最完整的源码树。

输出目录结构：
  <outdir>/
    .git/          — 缓存的 index 等元数据文件
    extracted/     — 还原的真实源码目录结构`,

	Example: `  # 对单个目标提取 Git 源码
  dumpall-go git-extract -u http://example.com/

  # 指定输出目录
  dumpall-go git-extract -u http://example.com/ -o ./leaked-src

  # 通过 SOCKS5 代理提取
  dumpall-go git-extract -u http://example.com/ -p socks5://127.0.0.1:1080`,

	DisableFlagsInUseLine: true,
	DisableAutoGenTag:     true,

	Run: func(cmd *cobra.Command, args []string) {
		if gitExtractURL == "" {
			color.Red("错误: 必须通过 -u/--url 指定目标 URL")
			cmd.Help()
			return
		}

		if err := utils.ValidateURL(gitExtractURL); err != nil {
			color.Red("URL 格式错误: %v", err)
			return
		}

		outdir := gitExtractOutdir
		if outdir == "" {
			outdir = filepath.Join("output", utils.GetHostname(gitExtractURL))
		}

		if err := os.MkdirAll(outdir, 0755); err != nil {
			color.Red("创建输出目录失败: %v", err)
			return
		}

		color.Cyan("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		color.Cyan("  Git Extract — 源代码提取")
		color.Cyan("  目标: %s", gitExtractURL)
		color.Cyan("  输出: %s", outdir)
		if gitExtractProxy != "" {
			color.Cyan("  代理: %s", gitExtractProxy)
		}
		color.Cyan("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		gitDumper := git.NewGitDumper()
		err := gitDumper.Extract(
			gitExtractURL,
			outdir,
			gitExtractProxy,
			func(url string, statusCode int, msg string) {
				if statusCode == 200 {
					fmt.Printf("  [200] %s\n", url)
				}
			},
		)
		if err != nil {
			color.Red("\n提取失败: %v", err)
			return
		}

		extractedDir := filepath.Join(outdir, "extracted")
		color.Green("\n提取成功！源代码已还原至: %s", extractedDir)
	},
}

func init() {
	gitExtractCmd.Flags().SortFlags = false

	gitExtractCmd.Flags().StringVarP(&gitExtractURL, "url", "u", "", "目标 URL（必填，例如: http://example.com/）")
	gitExtractCmd.Flags().StringVarP(&gitExtractOutdir, "outdir", "o", "", "输出目录（默认: output/<hostname>）")
	gitExtractCmd.Flags().StringVarP(&gitExtractProxy, "proxy", "p", "", "代理服务器 (支持: http://host:port | socks5://host:port | socks5h://host:port)")
	gitExtractCmd.Flags().IntVarP(&gitExtractWorkers, "workers", "w", 10, "并发下载线程数")

	RootCmd.AddCommand(gitExtractCmd)
}
