package git

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// IndexEntry 表示 .git/index 文件中的一条记录
// 参考 Git 官方文档: https://github.com/git/git/blob/master/Documentation/gitformat-index.txt
type IndexEntry struct {
	SHA1 string // 40 位十六进制 blob 对象哈希
	Name string // 文件相对路径（相对于工作区根目录）
}

// ParseIndexFile 解析 .git/index 二进制文件，返回所有被跟踪文件的 (sha1, path) 列表。
//
// index 文件结构（version 2/3/4 通用头部）：
//
//	4 字节   签名 "DIRC"
//	4 字节   版本号（2、3 或 4，大端序 uint32）
//	4 字节   条目数量（大端序 uint32）
//	<条目 × N>
//	...（扩展区、20字节 SHA1 校验和，此处不需要）
//
// 每条 entry（version 2 固定头长度 62 字节 + 变长 name + padding）：
//
//	ctime (8B) + mtime (8B) + dev(4B) + ino(4B) + mode(4B) +
//	uid(4B) + gid(4B) + size(4B) + sha1(20B) + flags(2B) = 62 字节
//	之后是 name（长度由 flags 低 12 位给出，或以 \0 结尾）
//	整个 entry 按 8 字节对齐，末尾补 \0
//
// version 4 使用路径前缀压缩（varint + 差分编码），此处按需支持。
func ParseIndexFile(path string) ([]IndexEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 index 文件失败: %v", err)
	}
	defer f.Close()

	r := bufio.NewReader(f)

	// 4 字节签名
	sig := make([]byte, 4)
	if _, err := io.ReadFull(r, sig); err != nil {
		return nil, fmt.Errorf("读取签名失败: %v", err)
	}
	if string(sig) != "DIRC" {
		return nil, fmt.Errorf("不是有效的 git index 文件（签名=%q）", string(sig))
	}

	version, err := readUint32(r)
	if err != nil {
		return nil, fmt.Errorf("读取版本号失败: %v", err)
	}
	if version < 2 || version > 4 {
		return nil, fmt.Errorf("不支持的 index 版本: %d", version)
	}

	numEntries, err := readUint32(r)
	if err != nil {
		return nil, fmt.Errorf("读取条目数量失败: %v", err)
	}

	entries := make([]IndexEntry, 0, numEntries)
	prevName := "" // version 4 前缀压缩需要用到上一个 entry 的完整名字

	for i := uint32(0); i < numEntries; i++ {
		entry, consumed, err := readIndexEntry(r, version, prevName)
		if err != nil {
			return entries, fmt.Errorf("解析第 %d 条 entry 失败: %v（已解析 %d 条，返回部分结果）", i, err, len(entries))
		}
		entries = append(entries, entry)
		prevName = entry.Name
		_ = consumed
	}

	return entries, nil
}

// readIndexEntry 读取单条 index entry，返回 entry 内容
func readIndexEntry(r *bufio.Reader, version uint32, prevName string) (IndexEntry, int, error) {
	total := 0

	skip := func(n int) error {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		total += n
		return nil
	}

	// ctime(8) + mtime(8) + dev(4) + ino(4) + mode(4) + uid(4) + gid(4) + size(4) = 40 字节
	if err := skip(8 + 8 + 4 + 4 + 4 + 4 + 4 + 4); err != nil {
		return IndexEntry{}, total, fmt.Errorf("读取固定头失败: %v", err)
	}

	// 20 字节 SHA1
	sha1Buf := make([]byte, 20)
	if _, err := io.ReadFull(r, sha1Buf); err != nil {
		return IndexEntry{}, total, fmt.Errorf("读取 sha1 失败: %v", err)
	}
	total += 20
	sha1 := hex.EncodeToString(sha1Buf)

	// 2 字节 flags
	flags, err := readUint16(r)
	if err != nil {
		return IndexEntry{}, total, fmt.Errorf("读取 flags 失败: %v", err)
	}
	total += 2

	extended := flags&0x4000 != 0
	nameLen := int(flags & 0x0FFF) // 低 12 位

	// version 2/3 中如果 extended 且 version==3，还有 2 字节 extra-flags
	if extended && version == 3 {
		if _, err := readUint16(r); err != nil {
			return IndexEntry{}, total, fmt.Errorf("读取 extra-flags 失败: %v", err)
		}
		total += 2
	}

	var name string
	if version == 4 {
		// version 4: 路径前缀压缩
		// 格式：varint(需要从 prevName 尾部删除的字节数) + 剩余的新增字符串（以 \0 结尾）
		stripLen, n, err := readVarint(r)
		if err != nil {
			return IndexEntry{}, total, fmt.Errorf("读取路径压缩长度失败: %v", err)
		}
		total += n

		suffix, m, err := readCString(r)
		if err != nil {
			return IndexEntry{}, total, fmt.Errorf("读取路径后缀失败: %v", err)
		}
		total += m

		keepLen := len(prevName) - int(stripLen)
		if keepLen < 0 {
			keepLen = 0
		}
		if keepLen > len(prevName) {
			keepLen = len(prevName)
		}
		name = prevName[:keepLen] + suffix
		// version 4 没有 8 字节对齐 padding
		return IndexEntry{SHA1: sha1, Name: name}, total, nil
	}

	// version 2/3：定长头部固定 62 字节（不含 extra-flags），name 长度由 flags 低 12 位给出；
	// 若 nameLen == 0xFFF，说明真实长度 >= 4095，需要按 \0 结尾读取。
	if nameLen < 0xFFF {
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(r, nameBuf); err != nil {
			return IndexEntry{}, total, fmt.Errorf("读取文件名失败: %v", err)
		}
		total += nameLen
		name = string(nameBuf)
	} else {
		s, n, err := readCString(r)
		if err != nil {
			return IndexEntry{}, total, fmt.Errorf("读取超长文件名失败: %v", err)
		}
		total += n
		name = s
	}

	// entry 总长需按 8 字节对齐，不足补 \0（62 字节固定头 + extra-flags(可选) + name + \0）
	entryLen := 62
	if extended && version == 3 {
		entryLen += 2
	}
	entryLen += len(name)
	padLen := 8 - (entryLen % 8)
	if padLen == 0 {
		padLen = 8
	}
	if err := skip(padLen); err != nil {
		return IndexEntry{}, total, fmt.Errorf("读取 padding 失败: %v", err)
	}

	return IndexEntry{SHA1: sha1, Name: name}, total, nil
}

func readUint32(r io.Reader) (uint32, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf), nil
}

func readUint16(r io.Reader) (uint16, error) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(buf), nil
}

// readVarint 读取 git 变长整数编码（每字节最高位为延续标志）
func readVarint(r *bufio.Reader) (uint64, int, error) {
	var result uint64
	n := 0
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, n, err
		}
		n++
		result = (result << 7) | uint64(b&0x7F)
		if b&0x80 == 0 {
			break
		}
		result++ // git varint 编码的特殊偏移规则（offset encoding）
	}
	return result, n, nil
}

// readCString 读取以 \0 结尾的字符串
func readCString(r *bufio.Reader) (string, int, error) {
	s, err := r.ReadString(0x00)
	if err != nil {
		return "", len(s), err
	}
	return s[:len(s)-1], len(s), nil
}
