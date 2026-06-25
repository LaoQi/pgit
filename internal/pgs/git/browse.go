package git

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RefInfo 是带元数据的 ref 视图，供浏览 API 使用。
type RefInfo struct {
	Name      string // short name，如 "master"、"v1.0"
	FullName  string // 完整 ref 名，如 "refs/heads/master"
	Type      string // ref 指向的对象类型：commit / tag / blob / tree
	Oid       Oid
	Author    string // committer 或 tagger 的名字
	Email     string
	Timestamp int64 // committer 或 tagger 的 Unix 时间戳
	Subject   string // message 首行
}

// treeMode 是 tree entry 中目录的 mode。
const treeMode uint32 = 0o040000

// isTreeEntry 判断 tree entry 是否为子目录。
func isTreeEntry(mode uint32) bool { return mode == treeMode }

// shortRefName 将完整 ref 名转为 short 形式（与 git refname:short 近似）：
// 去掉 refs/heads/、refs/tags/、refs/remotes/ 前缀，否则去掉 refs/ 前缀。
func shortRefName(name string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/", "refs/remotes/"} {
		if strings.HasPrefix(name, p) {
			return name[len(p):]
		}
	}
	return strings.TrimPrefix(name, "refs/")
}

// firstLine 返回 message 的首行（去掉末尾换行）。
func firstLine(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		return msg[:i]
	}
	return strings.TrimRight(msg, "\r")
}

// resolveRefName 将 ref 名（含 short 形式）解析为 oid。
// 依次尝试：原样、refs/heads/、refs/tags/、refs/。
func resolveRefName(rs *RefStore, name string) (Oid, error) {
	if oid, err := rs.Get(name); err == nil {
		return oid, nil
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/"} {
		if oid, err := rs.Get(prefix + name); err == nil {
			return oid, nil
		}
	}
	return "", fmt.Errorf("ref %q not found", name)
}

// derefToTree 将 oid 解引用到 tree oid。
// commit → 取其 tree；tag → 递归解引用到 commit/tree；tree → 直接返回。
// 返回 (commitOid, treeOid)。commitOid 为空表示输入直接是 tree（无 commit 时间戳）。
// 深度限制 16 防 tag 循环。
func derefToTree(store *LooseStore, oid Oid) (commitOid, treeOid Oid, err error) {
	const maxDepth = 16
	cur := oid
	for depth := 0; depth < maxDepth; depth++ {
		obj, rerr := store.Read(cur)
		if rerr != nil {
			return "", "", fmt.Errorf("deref %s: %w", cur, rerr)
		}
		switch obj.Type {
		case ObjCommit:
			c, perr := ParseCommit(obj.Content)
			if perr != nil {
				return "", "", fmt.Errorf("deref %s: parse commit: %w", cur, perr)
			}
			return cur, c.Tree, nil
		case ObjTag:
			tg, perr := ParseTag(obj.Content)
			if perr != nil {
				return "", "", fmt.Errorf("deref %s: parse tag: %w", cur, perr)
			}
			cur = tg.Object
			continue
		case ObjTree:
			return "", cur, nil
		default:
			return "", "", fmt.Errorf("object %s is %s, not commit/tag/tree", cur, obj.Type)
		}
	}
	return "", "", fmt.Errorf("deref %s: too many tag levels", oid)
}

// ResolveTreeIsh 将 treeIsh（ref 名 / HEAD / 40hex oid）解析为 tree oid。
// 返回 (commitOid, treeOid)。commitOid 为空表示 treeIsh 直接指向 tree。
// 40hex 当作对象 oid 处理（commit/tag/tree）；否则当作 ref 名（含 short）解析。
func ResolveTreeIsh(repoRoot, treeIsh string) (commitOid, treeOid Oid, err error) {
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	if Oid(treeIsh).Valid() {
		return derefToTree(store, Oid(treeIsh))
	}
	rs := NewRefStore(repoRoot)
	oid, err := resolveRefName(rs, treeIsh)
	if err != nil {
		return "", "", err
	}
	return derefToTree(store, oid)
}

// TreeAt 读取 treeOid 下指定 path 的 tree 条目。
// path 为空返回 treeOid 自身的条目；否则逐段下探。
// 任一非末段不是 tree 或路径不存在均返回错误。
func TreeAt(store *LooseStore, treeOid Oid, path string) ([]TreeEntry, error) {
	cur := treeOid
	if path != "" {
		for _, seg := range strings.Split(path, "/") {
			if seg == "" {
				continue
			}
			obj, err := store.Read(cur)
			if err != nil {
				return nil, fmt.Errorf("tree %s: %w", cur, err)
			}
			if obj.Type != ObjTree {
				return nil, fmt.Errorf("object %s is %s, not a tree", cur, obj.Type)
			}
			tr, err := ParseTree(obj.Content)
			if err != nil {
				return nil, fmt.Errorf("parse tree %s: %w", cur, err)
			}
			next := Oid("")
			for _, e := range tr.Entries {
				if e.Name == seg {
					next = e.Oid
					break
				}
			}
			if next == "" {
				return nil, fmt.Errorf("path %q not found under %s", seg, cur)
			}
			cur = next
		}
	}
	obj, err := store.Read(cur)
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", cur, err)
	}
	if obj.Type != ObjTree {
		return nil, fmt.Errorf("object %s is %s, not a tree", cur, obj.Type)
	}
	tr, err := ParseTree(obj.Content)
	if err != nil {
		return nil, fmt.Errorf("parse tree %s: %w", cur, err)
	}
	return tr.Entries, nil
}

// BlobAt 读取 treeOid 下指定 path 的 blob 对象。
// path 必须非空；逐段下探，末段定位 blob。
func BlobAt(store *LooseStore, treeOid Oid, path string) (*RawObject, error) {
	if path == "" {
		return nil, fmt.Errorf("blob path is empty")
	}
	segs := strings.Split(path, "/")
	cur := treeOid
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		obj, err := store.Read(cur)
		if err != nil {
			return nil, fmt.Errorf("traverse %s: %w", cur, err)
		}
		if obj.Type != ObjTree {
			return nil, fmt.Errorf("object %s is %s, not a tree", cur, obj.Type)
		}
		tr, err := ParseTree(obj.Content)
		if err != nil {
			return nil, fmt.Errorf("parse tree %s: %w", cur, err)
		}
		next := Oid("")
		for _, e := range tr.Entries {
			if e.Name == seg {
				next = e.Oid
				break
			}
		}
		if next == "" {
			return nil, fmt.Errorf("path %q not found under %s", seg, cur)
		}
		if i == len(segs)-1 {
			blob, err := store.Read(next)
			if err != nil {
				return nil, fmt.Errorf("blob %s: %w", next, err)
			}
			if blob.Type != ObjBlob {
				return nil, fmt.Errorf("object %s is %s, not a blob", next, blob.Type)
			}
			return blob, nil
		}
		cur = next
	}
	return nil, fmt.Errorf("path %q resolves to no blob", path)
}

// ForEachRefs 列出仓库所有 ref（不含 HEAD）及其元数据。
// ref 指向的对象读取失败时该 ref 被跳过（损坏仓库不阻断整体枚举）。
func ForEachRefs(repoRoot string) ([]RefInfo, error) {
	rs := NewRefStore(repoRoot)
	refs, err := rs.List()
	if err != nil {
		return nil, fmt.Errorf("for-each-ref: list: %w", err)
	}
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	out := make([]RefInfo, 0, len(refs))
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		info := RefInfo{
			FullName: r.Name,
			Name:     shortRefName(r.Name),
			Oid:      r.Oid,
		}
		if r.Oid.IsZero() {
			out = append(out, info)
			continue
		}
		obj, rerr := store.Read(r.Oid)
		if rerr != nil {
			out = append(out, info)
			continue
		}
		info.Type = string(obj.Type)
		switch obj.Type {
		case ObjCommit:
			if c, perr := ParseCommit(obj.Content); perr == nil {
				info.Author = c.Committer.Name
				info.Email = c.Committer.Email
				info.Timestamp = c.Committer.Timestamp
				info.Subject = firstLine(c.Message)
			}
		case ObjTag:
			if tg, perr := ParseTag(obj.Content); perr == nil {
				info.Author = tg.Tagger.Name
				info.Email = tg.Tagger.Email
				info.Timestamp = tg.Tagger.Timestamp
				info.Subject = firstLine(tg.Message)
			}
		}
		out = append(out, info)
	}
	return out, nil
}
