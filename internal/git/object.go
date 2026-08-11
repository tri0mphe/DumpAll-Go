package git

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// GitObject 表示解压后的一个 git 对象
type GitObject struct {
	Type string // "blob" | "tree" | "commit" | "tag"
	Size int
	Data []byte // 去除 header 后的原始内容
}

// TreeEntry 表示 tree 对象中的一条记录
type TreeEntry struct {
	Mode string // 文件模式，如 "100644"（普通文件）、"40000"（目录）、"120000"（软链接）
	Name string // 文件/目录名（不含路径前缀）
	SHA1 string // 40 位十六进制哈希，指向 blob 或子 tree
}

var objHeaderRe = regexp.MustCompile(`^(blob|tree|commit|tag) (\d+)\x00`)

// fetchLooseObject 从目标下载一个 loose object（.git/objects/xx/yyyy...），
// zlib 解压后解析出 "<type> <size>\0<content>" 格式，返回类型和纯内容。
//
// Git object 存储格式（loose object）：
//
//	zlib_compress("<type> <size>\0<content>")
//
// 其中 type 为 blob/tree/commit/tag，content 视 type 而定。
func fetchLooseObject(client *http.Client, base string, sha1 string) (*GitObject, error) {
	if len(sha1) != 40 {
		return nil, fmt.Errorf("非法 sha1 长度: %q", sha1)
	}
	objURL := base + ".git/objects/" + sha1[:2] + "/" + sha1[2:]

	resp, err := client.Get(objURL)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}

	return decodeLooseObject(raw)
}

// decodeLooseObject 对 loose object 原始字节做 zlib 解压并解析 header
func decodeLooseObject(raw []byte) (*GitObject, error) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("zlib 解压失败: %v", err)
	}
	defer zr.Close()

	decompressed, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("读取解压内容失败: %v", err)
	}

	m := objHeaderRe.FindSubmatchIndex(decompressed)
	if m == nil {
		return nil, fmt.Errorf("无法识别的 git object header")
	}
	objType := string(decompressed[m[2]:m[3]])
	sizeStr := string(decompressed[m[4]:m[5]])
	size, _ := strconv.Atoi(sizeStr)
	content := decompressed[m[1]:]

	return &GitObject{Type: objType, Size: size, Data: content}, nil
}

// ParseTreeObject 解析 tree 对象的二进制内容，返回条目列表。
//
// tree 对象格式（重复条目，无分隔符）：
//
//	"<mode> <name>\0" + <20字节原始SHA1>
//
// mode 是 ASCII 数字字符串（如 "100644"），name 以 \0 结尾，
// 紧跟着固定 20 字节的原始（非十六进制）SHA1。
func ParseTreeObject(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	for len(data) > 0 {
		// 找 "<mode> <name>\0" 部分
		idx := bytes.IndexByte(data, 0)
		if idx < 0 {
			return entries, fmt.Errorf("tree 对象格式错误：找不到 NUL 分隔符")
		}
		header := string(data[:idx])
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 {
			return entries, fmt.Errorf("tree 对象格式错误：header=%q", header)
		}
		mode, name := parts[0], parts[1]

		data = data[idx+1:]
		if len(data) < 20 {
			return entries, fmt.Errorf("tree 对象格式错误：sha1 长度不足")
		}
		sha1 := hex.EncodeToString(data[:20])
		data = data[20:]

		entries = append(entries, TreeEntry{Mode: mode, Name: name, SHA1: sha1})
	}
	return entries, nil
}

// ExtractCommitTreeSHA1 从 commit 对象内容中解析出根 tree 的 sha1。
// commit 对象是纯文本格式，第一行形如: "tree <sha1>"
func ExtractCommitTreeSHA1(commitData []byte) (string, error) {
	lines := strings.Split(string(commitData), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "tree ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "tree ")), nil
		}
		if line == "" {
			break
		}
	}
	return "", fmt.Errorf("commit 对象中未找到 tree 字段")
}

// ExtractCommitParents 从 commit 对象内容中解析出所有 parent commit 的 sha1（可能有多个，即 merge commit）
func ExtractCommitParents(commitData []byte) []string {
	var parents []string
	lines := strings.Split(string(commitData), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(strings.TrimPrefix(line, "parent ")))
		}
		if line == "" {
			break
		}
	}
	return parents
}

// isTreeDir 判断 tree entry 的 mode 是否为目录
func isTreeDir(mode string) bool {
	// 目录固定为 "40000"（有些实现写作 "040000"）
	return mode == "40000" || mode == "040000"
}

// isGitlink 判断 tree entry 是否为 submodule (gitlink)，其 sha1 指向另一仓库的 commit，不应下载
func isGitlink(mode string) bool {
	return mode == "160000"
}
