package git

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Ref 表示一个引用
type Ref struct {
	Name   string  // 完整名如 "refs/heads/master"，或 "HEAD"
	Oid    Oid
	Symref *string // 非 nil 表示符号引用（指向另一个 ref 名）
}

// RefUpdate 用于 receive-pack 的 ref 更新
type RefUpdate struct {
	Name   string
	OldOid Oid // CAS 期望值，ZeroOid 表示「必须不存在」
	NewOid Oid // ZeroOid 表示删除
}

// RefUpdateResult 单个 ref 更新的结果
type RefUpdateResult struct {
	Name   string
	Ok     bool
	Reason string // 失败时填
}

// RefStore 管理仓库的 refs
type RefStore struct {
	Root string // 仓库根目录（含 HEAD/refs/packed-refs）
	mu   sync.Mutex
}

// NewRefStore 构造 RefStore
func NewRefStore(repoRoot string) *RefStore {
	return &RefStore{Root: repoRoot}
}

// Head 解析 HEAD。返回 symref 指向的 ref 名（如 "refs/heads/master"）；
// 若 detached（直接 oid）返回 ""。HEAD 文件不存在时返回错误。
func (s *RefStore) Head() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, "HEAD"))
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "ref: ") {
		return strings.TrimSpace(line[len("ref: "):]), nil
	}
	return "", nil // detached
}

// parsePackedRefs 解析 <Root>/packed-refs，返回 refname→oid map。
// 文件不存在返回空 map 不报错。`^<oid>` peeled 行忽略，`#` 注释行忽略。
func (s *RefStore) parsePackedRefs() (map[string]Oid, error) {
	m := make(map[string]Oid)
	f, err := os.Open(filepath.Join(s.Root, "packed-refs"))
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		m[line[sp+1:]] = Oid(line[:sp])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// List 列出所有 refs。先解析 packed-refs 建基础 map，再扫 <Root>/refs/ 下
// 所有 loose refs 覆盖（同名 loose 优先）。结果按 Name 排序。
// HEAD 作为特殊 ref 单独加入列表（symref 指向或 detached oid）。
func (s *RefStore) List() ([]Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, err := s.parsePackedRefs()
	if err != nil {
		return nil, err
	}

	// 扫描 loose refs 覆盖 packed
	refsDir := filepath.Join(s.Root, "refs")
	err = filepath.Walk(refsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // refs 目录不存在（空仓库）
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.Root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		m[name] = Oid(strings.TrimSpace(string(data)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	refs := make([]Ref, 0, len(m)+1)
	for name, oid := range m {
		refs = append(refs, Ref{Name: name, Oid: oid})
	}

	// HEAD 作为特殊 ref（symref 时附带目标 ref 的 oid，目标不存在则 ZeroOid）
	if data, err := os.ReadFile(filepath.Join(s.Root, "HEAD")); err == nil {
		line := strings.TrimSpace(string(data))
		if strings.HasPrefix(line, "ref: ") {
			target := strings.TrimSpace(line[len("ref: "):])
			oid := m[target]
			if oid == "" {
				oid = ZeroOid
			}
			refs = append(refs, Ref{Name: "HEAD", Oid: oid, Symref: &target})
		} else if line != "" {
			refs = append(refs, Ref{Name: "HEAD", Oid: Oid(line)})
		}
	}

	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// Get 取单个 ref 的 oid。优先 loose（<Root>/<name> 文件），其次 packed-refs。
// 支持 symref 跟随（如 HEAD 指向 refs/heads/master）。未找到返回错误。
func (s *RefStore) Get(name string) (Oid, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getOid(name)
}

// getOid 内部取 oid，不加锁（调用方持锁）。支持 symref 跟随。
func (s *RefStore) getOid(name string) (Oid, error) {
	path := filepath.Join(s.Root, name)
	data, err := os.ReadFile(path)
	if err == nil {
		line := strings.TrimSpace(string(data))
		if strings.HasPrefix(line, "ref: ") {
			target := strings.TrimSpace(line[len("ref: "):])
			return s.getOid(target) // 跟随 symref
		}
		return Oid(line), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	m, err := s.parsePackedRefs()
	if err != nil {
		return "", err
	}
	if oid, ok := m[name]; ok {
		return oid, nil
	}
	return "", fmt.Errorf("ref %q not found", name)
}

// readCurrentOid 读 ref 现值（loose 没有则查 packed-refs；都没有 ZeroOid）。
// 支持 symref 跟随。供 Update CAS 校验用。
func (s *RefStore) readCurrentOid(name string) (Oid, error) {
	path := filepath.Join(s.Root, name)
	data, err := os.ReadFile(path)
	if err == nil {
		line := strings.TrimSpace(string(data))
		if strings.HasPrefix(line, "ref: ") {
			target := strings.TrimSpace(line[len("ref: "):])
			return s.readCurrentOid(target)
		}
		return Oid(line), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	m, err := s.parsePackedRefs()
	if err != nil {
		return "", err
	}
	if oid, ok := m[name]; ok {
		return oid, nil
	}
	return ZeroOid, nil
}

// Update per-ref 原子更新。对每个 update：创建 lock 文件、读现值、CAS 校验、
// 写入或删除、释放 lock。任一 ref 失败不影响其他 ref（per-ref 原子，与 git 一致）。
// 返回每个 ref 的结果；err 仅在系统级错误时返回。
func (s *RefStore) Update(updates []RefUpdate) ([]RefUpdateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]RefUpdateResult, 0, len(updates))
	for _, u := range updates {
		results = append(results, s.updateOne(u))
	}
	return results, nil
}

// updateOne 处理单个 ref 更新。
func (s *RefStore) updateOne(u RefUpdate) RefUpdateResult {
	path := filepath.Join(s.Root, u.Name)
	lockPath := path + ".lock"

	// 父目录可能不存在（ref 名含斜杠如 refs/heads/feature/x）
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return RefUpdateResult{Name: u.Name, Reason: "mkdir: " + err.Error()}
	}

	// 创建 lock 文件（O_EXCL 防并发竞争）
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		return RefUpdateResult{Name: u.Name, Reason: "lock: " + err.Error()}
	}
	defer os.Remove(lockPath) // rename 成功后此 Remove 无效；失败时清理

	// 读现值
	currentOid, err := s.readCurrentOid(u.Name)
	if err != nil {
		lf.Close()
		return RefUpdateResult{Name: u.Name, Reason: "read current: " + err.Error()}
	}

	// CAS 校验
	if u.OldOid == ZeroOid {
		if currentOid != ZeroOid {
			lf.Close()
			return RefUpdateResult{Name: u.Name, Reason: "ref already exists"}
		}
	} else {
		if currentOid != u.OldOid {
			lf.Close()
			return RefUpdateResult{Name: u.Name, Reason: "non-fast-forward"}
		}
	}

	// 删除 ref
	if u.NewOid == ZeroOid {
		lf.Close()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return RefUpdateResult{Name: u.Name, Reason: "remove: " + err.Error()}
		}
		return RefUpdateResult{Name: u.Name, Ok: true}
	}

	// 写入新 oid 到 lock 文件
	if _, err := fmt.Fprintf(lf, "%s\n", u.NewOid); err != nil {
		lf.Close()
		return RefUpdateResult{Name: u.Name, Reason: "write: " + err.Error()}
	}
	if err := lf.Close(); err != nil {
		return RefUpdateResult{Name: u.Name, Reason: "close: " + err.Error()}
	}

	// rename lock → 目标（原子替换）
	if err := os.Rename(lockPath, path); err != nil {
		return RefUpdateResult{Name: u.Name, Reason: "rename: " + err.Error()}
	}
	return RefUpdateResult{Name: u.Name, Ok: true}
}

// SetHead atomically sets HEAD to a symbolic reference target.
// target must be a full ref name like "refs/heads/master".
func (s *RefStore) SetHead(target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.Root, "HEAD")
	lockPath := path + ".lock"

	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer os.Remove(lockPath)

	content := fmt.Sprintf("ref: %s\n", target)
	if _, err := lf.WriteString(content); err != nil {
		return err
	}
	if err := lf.Close(); err != nil {
		return err
	}

	return os.Rename(lockPath, path)
}
