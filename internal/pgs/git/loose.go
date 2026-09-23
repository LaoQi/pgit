package git

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ObjectStore 是仓库对象存储抽象：读、存在性判断、写。
// 当前唯一实现是 LooseStore（全松散对象）；packfile 后端、缓存层、
// 对象配额等后续实现只需满足本接口即可接入 git 协议层与浏览 API。
type ObjectStore interface {
	Read(oid Oid) (*RawObject, error)
	Exists(oid Oid) bool
	Write(obj *RawObject) (Oid, error)
	// Stat 只读取对象头部（type 与 size），不解压整个内容。
	// 供 clone 侧「先规划、后编码」两遍式遍历使用，避免对象内容全量驻留内存。
	Stat(oid Oid) (ObjectType, int, error)
}

// NewObjectStore 打开仓库的松散对象存储（<repoRoot>/objects）。
func NewObjectStore(repoRoot string) ObjectStore {
	return &LooseStore{Root: filepath.Join(repoRoot, "objects")}
}

// LooseStore 松散对象存储，对应 <repo>/objects 目录
type LooseStore struct {
	Root string // objects 目录绝对路径
}

var _ ObjectStore = (*LooseStore)(nil)

// Path 返回 oid 对应的 loose 文件路径
func (s *LooseStore) Path(oid Oid) string {
	return filepath.Join(s.Root, string(oid[:2]), string(oid[2:]))
}

// Exists 判断 oid 是否存在
func (s *LooseStore) Exists(oid Oid) bool {
	_, err := os.Stat(s.Path(oid))
	return err == nil
}

// Read 读取并解压 loose 对象
func (s *LooseStore) Read(oid Oid) (*RawObject, error) {
	f, err := os.Open(s.Path(oid))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("loose %s: zlib init: %w", oid, err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("loose %s: zlib read: %w", oid, err)
	}
	// 解析 header: "<type> <size>\0"
	nul := bytes.IndexByte(raw, 0)
	if nul < 0 {
		return nil, fmt.Errorf("loose %s: no header terminator", oid)
	}
	header := string(raw[:nul])
	sp := bytes.IndexByte(raw[:nul], ' ')
	if sp < 0 {
		return nil, fmt.Errorf("loose %s: bad header %q", oid, header)
	}
	objType := ObjectType(raw[:sp])
	var size int
	if _, err := fmt.Sscanf(string(raw[sp+1:nul]), "%d", &size); err != nil {
		return nil, fmt.Errorf("loose %s: bad size: %w", oid, err)
	}
	content := raw[nul+1:]
	if len(content) != size {
		return nil, fmt.Errorf("loose %s: size mismatch header=%d actual=%d", oid, size, len(content))
	}
	return &RawObject{Type: objType, Size: size, Content: content}, nil
}

// Stat 解析 loose 对象头部，返回 type 与 content 长度，不加载 content。
// zlib 流中 header 位于最前，读到 NUL 即可停止。
func (s *LooseStore) Stat(oid Oid) (ObjectType, int, error) {
	f, err := os.Open(s.Path(oid))
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return "", 0, fmt.Errorf("loose %s: zlib init: %w", oid, err)
	}
	defer zr.Close()

	// 头部格式 "<type> <size>\0"，长度有限（type ≤ 6 + 空格 + 数字），最多读 64 字节
	var head []byte
	buf := make([]byte, 1)
	for len(head) < 64 {
		n, err := zr.Read(buf)
		if n > 0 {
			head = append(head, buf[0])
			if buf[0] == 0 {
				break
			}
		}
		if err != nil {
			return "", 0, fmt.Errorf("loose %s: read header: %w", oid, err)
		}
	}
	nul := bytes.IndexByte(head, 0)
	if nul < 0 {
		return "", 0, fmt.Errorf("loose %s: no header terminator", oid)
	}
	sp := bytes.IndexByte(head[:nul], ' ')
	if sp < 0 {
		return "", 0, fmt.Errorf("loose %s: bad header %q", oid, head[:nul])
	}
	var size int
	if _, err := fmt.Sscanf(string(head[sp+1:nul]), "%d", &size); err != nil {
		return "", 0, fmt.Errorf("loose %s: bad size: %w", oid, err)
	}
	return ObjectType(head[:sp]), size, nil
}

// Write 写入 loose 对象。先写临时文件再 rename，原子性。
// 若 oid 已存在则视为成功（幂等）。
func (s *LooseStore) Write(obj *RawObject) (Oid, error) {
	oid := obj.Oid()
	if s.Exists(oid) {
		return oid, nil
	}
	// 构造 zlib(header + content)
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(obj.Header())
	zw.Write(obj.Content)
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("loose write %s: zlib close: %w", oid, err)
	}
	path := s.Path(oid)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("loose write %s: mkdir: %w", oid, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("loose write %s: tmp: %w", oid, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 若 rename 成功则 Remove 无效
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return "", fmt.Errorf("loose write %s: write: %w", oid, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("loose write %s: close: %w", oid, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("loose write %s: rename: %w", oid, err)
	}
	return oid, nil
}
