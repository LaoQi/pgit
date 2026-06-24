package git

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LooseStore 松散对象存储，对应 <repo>/objects 目录
type LooseStore struct {
	Root string // objects 目录绝对路径
}

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
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
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
