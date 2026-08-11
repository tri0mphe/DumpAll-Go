package git

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var sha1Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// resolveHeadCommit 尝试从目标解析出当前 HEAD 指向的 commit sha1。
//
// 解析顺序：
//  1. 下载 .git/HEAD，如果内容是 40 位 sha1，直接返回（detached HEAD）
//  2. 否则内容形如 "ref: refs/heads/master"，再去下载该 ref 文件获取 sha1
//  3. 若 ref 文件不存在（已被 gc 打包），尝试从 .git/packed-refs 中查找同名 ref
func resolveHeadCommit(client *http.Client, base string) (string, error) {
	headData, err := fetchTextFile(client, base+".git/HEAD")
	if err != nil {
		return "", fmt.Errorf("下载 HEAD 失败: %v", err)
	}
	headData = strings.TrimSpace(headData)

	if sha1Re.MatchString(headData) {
		return headData, nil
	}

	const prefix = "ref: "
	if !strings.HasPrefix(headData, prefix) {
		return "", fmt.Errorf("无法识别的 HEAD 内容: %q", headData)
	}
	refPath := strings.TrimSpace(strings.TrimPrefix(headData, prefix))

	// 尝试直接下载该 ref 文件，例如 .git/refs/heads/master
	if refData, err := fetchTextFile(client, base+".git/"+refPath); err == nil {
		refData = strings.TrimSpace(refData)
		if sha1Re.MatchString(refData) {
			return refData, nil
		}
	}

	// ref 文件可能已被打包进 packed-refs，逐行查找匹配的 ref 名
	packedData, err := fetchTextFile(client, base+".git/packed-refs")
	if err != nil {
		return "", fmt.Errorf("ref 文件 %q 不可直接访问，且 packed-refs 也不可用: %v", refPath, err)
	}
	sha1, ok := lookupPackedRef(packedData, refPath)
	if !ok {
		return "", fmt.Errorf("在 packed-refs 中未找到 ref: %s", refPath)
	}
	return sha1, nil
}

// lookupPackedRef 在 packed-refs 文件内容中查找指定 ref 名对应的 sha1
//
// packed-refs 格式（每行一条）：
//
//	<40位sha1> <ref名称>
//
// 以 # 开头的是注释行，以 ^ 开头的是上一行标签的解引用（peeled）行，忽略。
func lookupPackedRef(data, refName string) (string, bool) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		if parts[1] == refName {
			return parts[0], true
		}
	}
	return "", false
}

// ListAllPackedRefsCommits 返回 packed-refs 中所有 ref 对应的 sha1（用于在 HEAD 解析失败时兜底扫描全部分支）
func ListAllPackedRefsCommits(data string) []string {
	var shas []string
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 2 && sha1Re.MatchString(parts[0]) {
			shas = append(shas, parts[0])
		}
	}
	return shas
}

// fetchTextFile 下载一个文本文件并返回其内容字符串
func fetchTextFile(client *http.Client, fileURL string) (string, error) {
	resp, err := client.Get(fileURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
