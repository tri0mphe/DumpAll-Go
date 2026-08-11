package git

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"dumpall-go/internal/dumper"
	"dumpall-go/pkg/utils"

	"github.com/fatih/color"
)

// Extract 从目标 URL 的 .git 信息泄露中还原完整源代码。
//
// 参照业界标准 GitHack (https://github.com/lijiejie/GitHack) 的核心思路，并做了增强：
//
//  1. index 模式（主策略，等价于 GitHack 的做法）：
//     下载 .git/index，解析出工作区所有被跟踪文件的 (name, sha1) 列表，
//     再从 .git/objects/<sha1[:2]>/<sha1[2:]> 逐个下载 blob 对象、zlib 解压、
//     去掉 "blob <size>\0" 头，还原为原始文件内容。
//     这是最快最准确的方式，但要求 .git/index 文件本身可访问。
//
//  2. tree 递归模式（增强策略，GitHack 不具备）：
//     当 index 不可用/不完整时（例如仅暴露了 .git/HEAD、.git/refs、.git/objects），
//     通过 HEAD → commit → tree 逐级解析：
//     - 解析 .git/HEAD 找到当前分支 ref，再从 ref 文件或 packed-refs 找到 commit sha1
//     - 下载 commit 对象，解析出根 tree 的 sha1
//     - 递归下载 tree 对象（tree 内部条目可能指向子 tree 或 blob），还原完整目录结构
//     这种方式只依赖 objects 目录本身可被遍历下载，鲁棒性更强。
//
//  3. 两种模式的结果会合并去重（以相对路径为 key），尽可能还原出最完整的源码树。
func (g *GitDumper) Extract(targetURL, outdir, proxyAddr string, progressCb dumper.ProgressCallback) error {
	color.Cyan("[Git-Extract] 开始提取源代码: %s", targetURL)

	client, err := utils.CreateHTTPClient(proxyAddr)
	if err != nil {
		return fmt.Errorf("[Git-Extract] 创建HTTP客户端失败: %v", err)
	}

	if !strings.HasSuffix(targetURL, "/") {
		targetURL += "/"
	}

	extractDir := filepath.Join(outdir, "extracted")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %v", err)
	}

	// 汇总结果：relPath -> sha1，用于两种模式合并去重
	fileMap := make(map[string]string)

	// ── 策略1：index 模式 ──────────────────────────────────────────────
	indexEntries, indexErr := extractViaIndex(client, targetURL, outdir)
	if indexErr != nil {
		color.Yellow("[Git-Extract] index 模式不可用: %v", indexErr)
	} else {
		color.Green("[Git-Extract] index 模式解析出 %d 个文件", len(indexEntries))
		for _, e := range indexEntries {
			fileMap[e.Name] = e.SHA1
		}
	}

	// ── 策略2：tree 递归模式（HEAD → commit → tree）─────────────────────
	treeFiles, treeErr := extractViaTree(client, targetURL)
	if treeErr != nil {
		color.Yellow("[Git-Extract] tree 递归模式不可用: %v", treeErr)
	} else {
		color.Green("[Git-Extract] tree 递归模式解析出 %d 个文件", len(treeFiles))
		for relPath, sha1 := range treeFiles {
			if _, exists := fileMap[relPath]; !exists {
				fileMap[relPath] = sha1
			}
		}
	}

	if len(fileMap) == 0 {
		return fmt.Errorf("[Git-Extract] 未能获取任何文件列表，请确认目标存在 .git/index 或 .git/HEAD+objects 泄露")
	}

	color.Cyan("[Git-Extract] 合并后共 %d 个文件，开始下载还原...", len(fileMap))

	successCount := 0
	failCount := 0
	for relPath, sha1 := range fileMap {
		localPath := filepath.Join(extractDir, filepath.FromSlash(relPath))

		obj, err := fetchLooseObject(client, targetURL, sha1)
		if err != nil {
			color.Red("[Git-Extract] ✗ 下载对象失败 %s (sha1=%s): %v", relPath, sha1, err)
			if progressCb != nil {
				progressCb(targetURL+".git/objects/"+sha1[:2]+"/"+sha1[2:], 0, "下载失败")
			}
			failCount++
			continue
		}
		if obj.Type != "blob" {
			color.Yellow("[Git-Extract] 跳过非 blob 对象 %s (type=%s)", relPath, obj.Type)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			color.Red("[Git-Extract] 创建目录失败 %s: %v", filepath.Dir(localPath), err)
			failCount++
			continue
		}
		if err := os.WriteFile(localPath, obj.Data, 0644); err != nil {
			color.Red("[Git-Extract] 写入文件失败 %s: %v", localPath, err)
			failCount++
			continue
		}

		color.Green("[Git-Extract] ✓ %s", relPath)
		if progressCb != nil {
			progressCb(targetURL+".git/objects/"+sha1[:2]+"/"+sha1[2:], http.StatusOK, localPath)
		}
		successCount++
	}

	color.Green("[Git-Extract] 提取完成！成功: %d，失败: %d，输出目录: %s", successCount, failCount, extractDir)
	return nil
}

// extractViaIndex 下载并解析 .git/index，返回所有条目
func extractViaIndex(client *http.Client, targetURL, outdir string) ([]IndexEntry, error) {
	indexURL := targetURL + ".git/index"
	color.Yellow("[Git-Extract] 尝试下载 index: %s", indexURL)

	gitDir := filepath.Join(outdir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 .git 缓存目录失败: %v", err)
	}
	indexLocal := filepath.Join(gitDir, "index")

	if err := downloadFile(client, indexURL, indexLocal); err != nil {
		return nil, fmt.Errorf("index 不可用: %v", err)
	}

	entries, err := ParseIndexFile(indexLocal)
	if err != nil && len(entries) == 0 {
		return nil, fmt.Errorf("解析 index 失败: %v", err)
	}
	// 部分解析失败时也返回已解析的条目（弹性处理，参照 svn entries 解析的容错思路）
	return entries, nil
}

// extractViaTree 通过 HEAD → commit → tree 递归解析出完整文件树
// 返回 map[相对路径]sha1
func extractViaTree(client *http.Client, targetURL string) (map[string]string, error) {
	commitSHA1, err := resolveHeadCommit(client, targetURL)
	if err != nil {
		return nil, fmt.Errorf("解析 HEAD 指向的 commit 失败: %v", err)
	}
	color.Cyan("[Git-Extract] HEAD -> commit %s", commitSHA1)

	commitObj, err := fetchLooseObject(client, targetURL, commitSHA1)
	if err != nil {
		return nil, fmt.Errorf("下载 commit 对象失败: %v", err)
	}
	if commitObj.Type != "commit" {
		return nil, fmt.Errorf("对象 %s 不是 commit 类型（实际: %s）", commitSHA1, commitObj.Type)
	}

	rootTreeSHA1, err := ExtractCommitTreeSHA1(commitObj.Data)
	if err != nil {
		return nil, fmt.Errorf("解析 commit 中的 tree 字段失败: %v", err)
	}
	color.Cyan("[Git-Extract] commit -> root tree %s", rootTreeSHA1)

	result := make(map[string]string)
	visited := make(map[string]bool) // 防止 tree 对象自引用死循环（正常仓库不会出现，做防御）
	if err := walkTree(client, targetURL, rootTreeSHA1, "", result, visited); err != nil {
		return result, fmt.Errorf("递归解析 tree 失败: %v", err)
	}
	return result, nil
}

// walkTree 递归下载并解析 tree 对象，将所有 blob 文件的相对路径和 sha1 写入 result
func walkTree(client *http.Client, targetURL, treeSHA1, pathPrefix string, result map[string]string, visited map[string]bool) error {
	if visited[treeSHA1] {
		return nil
	}
	visited[treeSHA1] = true

	treeObj, err := fetchLooseObject(client, targetURL, treeSHA1)
	if err != nil {
		return fmt.Errorf("下载 tree 对象 %s 失败: %v", treeSHA1, err)
	}
	if treeObj.Type != "tree" {
		return fmt.Errorf("对象 %s 不是 tree 类型（实际: %s）", treeSHA1, treeObj.Type)
	}

	entries, err := ParseTreeObject(treeObj.Data)
	if err != nil {
		return fmt.Errorf("解析 tree 对象 %s 失败: %v", treeSHA1, err)
	}

	for _, entry := range entries {
		relPath := entry.Name
		if pathPrefix != "" {
			relPath = pathPrefix + "/" + entry.Name
		}

		if isGitlink(entry.Mode) {
			// submodule，指向另一个仓库的 commit，跳过
			color.Yellow("[Git-Extract] 跳过 submodule: %s", relPath)
			continue
		}

		if isTreeDir(entry.Mode) {
			if err := walkTree(client, targetURL, entry.SHA1, relPath, result, visited); err != nil {
				color.Red("[Git-Extract] 目录 %s 解析失败: %v", relPath, err)
			}
			continue
		}

		// 普通文件 / 可执行文件 / 符号链接，都对应 blob 对象
		result[relPath] = entry.SHA1
	}

	return nil
}
