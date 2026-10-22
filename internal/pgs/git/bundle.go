package git

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// bundleMagic 是 git bundle 版本行。pgit 只生成 v2（无前置包、纯 ref 列表 + pack），
// 实测可被 git 2.x 的 `git bundle verify` / `git clone` / `git ls-remote` 直接消费。
const bundleMagic = "# v2 git bundle"

// BundleRef 是 bundle 头部记录的一条 ref（Name 为 "HEAD" 时 Oid 是该 ref 的指向）。
type BundleRef struct {
	Name string `json:"name"`
	Oid  Oid    `json:"oid"`
}

// BundleHeader 是解析出的 bundle 头部。
type BundleHeader struct {
	Version string      `json:"version"`
	Refs    []BundleRef `json:"refs"`
	// HeadOid 是头部 HEAD 行记录的 oid（零值表示 bundle 未记录 HEAD）。
	HeadOid Oid `json:"headOid,omitempty"`
}

// BundleInfo 是 WriteBundle 的统计结果。
type BundleInfo struct {
	Refs    []BundleRef `json:"refs"`
	Head    string      `json:"head,omitempty"`
	Objects int         `json:"objects"`
	Bytes   int64       `json:"bytes"`
}

// RestoreResult 是 RestoreBundle 的统计结果。
type RestoreResult struct {
	Objects int      `json:"objects"`
	Refs    []string `json:"refs"`
	Head    string   `json:"head,omitempty"`
}

// WriteBundle 把仓库当前 refs 可达的全部对象写成 git bundle（v2，无前置包 = 完整历史）。
//
// 头部格式（与 git 自身产出一致）：
//
//	# v2 git bundle
//	<oid> <refname>
//	...
//	<oid> HEAD          (仓库有 HEAD symref 且指向已存在的 ref 时)
//	<空行>
//	<packfile>
//
// 产物自带 pack trailer SHA1：解包侧（PackDecoder）会逐对象重算 SHA1 并校验 trailer。
// 调用方应写入临时文件并 fsync+rename，避免留下半成品。
func WriteBundle(w io.Writer, repoRoot string) (*BundleInfo, error) {
	rs := NewRefStore(repoRoot)
	store := NewObjectStore(repoRoot)

	refs, err := rs.List()
	if err != nil {
		return nil, fmt.Errorf("bundle: list refs: %w", err)
	}
	heads := make([]BundleRef, 0, len(refs))
	roots := make([]Oid, 0, len(refs))
	for _, r := range refs {
		if r.Name == "HEAD" || !r.Oid.Valid() || r.Oid.IsZero() || !safeRefName(r.Name) {
			continue
		}
		heads = append(heads, BundleRef{Name: r.Name, Oid: r.Oid})
		roots = append(roots, r.Oid)
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].Name < heads[j].Name })
	if len(heads) == 0 {
		return nil, fmt.Errorf("bundle: repository has no refs to bundle")
	}

	// HEAD 行：记录默认分支指向，恢复时据此重建 HEAD symref（git 的多分支 clone 也依赖它）。
	headTarget, _ := rs.Head()
	var headOid Oid
	if headTarget != "" {
		for _, h := range heads {
			if h.Name == headTarget {
				headOid = h.Oid
				break
			}
		}
	}

	metas, err := WalkReachable(store, roots, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("bundle: walk objects: %w", err)
	}

	cw := &countingWriter{w: w}
	if _, err := io.WriteString(cw, bundleMagic+"\n"); err != nil {
		return nil, fmt.Errorf("bundle: write header: %w", err)
	}
	for _, h := range heads {
		if _, err := fmt.Fprintf(cw, "%s %s\n", h.Oid, h.Name); err != nil {
			return nil, fmt.Errorf("bundle: write header: %w", err)
		}
	}
	if !headOid.IsZero() {
		if _, err := fmt.Fprintf(cw, "%s HEAD\n", headOid); err != nil {
			return nil, fmt.Errorf("bundle: write header: %w", err)
		}
	}
	if _, err := io.WriteString(cw, "\n"); err != nil {
		return nil, fmt.Errorf("bundle: write header: %w", err)
	}
	if err := encodePack(store, metas, cw); err != nil {
		return nil, fmt.Errorf("bundle: encode pack: %w", err)
	}

	info := &BundleInfo{Refs: heads, Objects: len(metas), Bytes: cw.n}
	if !headOid.IsZero() {
		info.Head = headTarget
	}
	return info, nil
}

// ReadBundle 解析 bundle 头部，返回头部信息与指向 pack 起点的 reader。
// r 会被 bufio 包装；调用方应使用返回的 reader 继续读 pack（不要再用原 r）。
func ReadBundle(r io.Reader) (*BundleHeader, *bufio.Reader, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	first, err := br.ReadString('\n')
	if err != nil {
		return nil, nil, fmt.Errorf("bundle: read magic: %w", err)
	}
	first = strings.TrimRight(first, "\r\n")
	if !strings.HasPrefix(first, "# v") || !strings.HasSuffix(first, " git bundle") {
		return nil, nil, fmt.Errorf("bundle: bad magic %q", first)
	}
	version := strings.TrimSuffix(strings.TrimPrefix(first, "# "), " git bundle")
	hdr := &BundleHeader{Version: version}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil, fmt.Errorf("bundle: unexpected EOF in header")
			}
			return nil, nil, fmt.Errorf("bundle: read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "-") {
			return nil, nil, fmt.Errorf("bundle: incremental bundles (prerequisites) are not supported")
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			return nil, nil, fmt.Errorf("bundle: bad header line %q", line)
		}
		oid, name := Oid(fields[0]), fields[1]
		if !oid.Valid() || oid.IsZero() {
			return nil, nil, fmt.Errorf("bundle: bad oid in header %q", line)
		}
		if name == "HEAD" {
			hdr.HeadOid = oid
			continue
		}
		if !safeRefName(name) {
			return nil, nil, fmt.Errorf("bundle: unsafe ref name %q", name)
		}
		hdr.Refs = append(hdr.Refs, BundleRef{Name: name, Oid: oid})
	}
	if len(hdr.Refs) == 0 {
		return nil, nil, fmt.Errorf("bundle: no refs in header")
	}
	return hdr, br, nil
}

// RestoreBundle 把 bundle 解包到 destRoot（对象以 loose 形式落盘，逐对象 SHA1 校验），
// 并把头部记录的 ref 写成 loose ref。已存在的同名 ref 会被覆盖（以归档为准）；
// 对象与已有对象按内容寻址天然幂等。
//
// headTarget 为归档记录的默认分支（可为空，此时用头部 HEAD 行的 oid 匹配 refs/heads/*）。
func RestoreBundle(bundlePath, destRoot, headTarget string) (*RestoreResult, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr, br, err := ReadBundle(f)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destRoot, 0o750); err != nil {
		return nil, fmt.Errorf("bundle: create repo dir: %w", err)
	}
	store := NewObjectStore(destRoot)
	dec := NewPackDecoder(br, store)
	objects, err := dec.DecodeTo(store)
	if err != nil {
		return nil, fmt.Errorf("bundle: decode pack: %w", err)
	}

	rs := NewRefStore(destRoot)
	current := make(map[string]Oid)
	if list, err := rs.List(); err == nil {
		for _, r := range list {
			current[r.Name] = r.Oid
		}
	}
	updates := make([]RefUpdate, 0, len(hdr.Refs))
	for _, r := range hdr.Refs {
		old := ZeroOid
		if cur, ok := current[r.Name]; ok {
			old = cur
		}
		updates = append(updates, RefUpdate{Name: r.Name, OldOid: old, NewOid: r.Oid})
	}
	results, err := rs.Update(updates)
	if err != nil {
		return nil, fmt.Errorf("bundle: write refs: %w", err)
	}
	res := &RestoreResult{Objects: objects}
	for i, u := range updates {
		if i < len(results) && results[i].Ok {
			res.Refs = append(res.Refs, u.Name)
		} else if i < len(results) {
			return nil, fmt.Errorf("bundle: write ref %s: %s", u.Name, results[i].Reason)
		}
	}

	target := resolveBundleHead(hdr, headTarget)
	if target != "" {
		if err := rs.SetHead(target); err == nil {
			res.Head = target
		}
	}
	InvalidateRefsCache(destRoot)
	return res, nil
}

// resolveBundleHead 决定恢复后的默认分支：优先归档记录的 target（且该 ref 在包内），
// 否则用 HEAD 行 oid 匹配 refs/heads/* 中字典序最小的一个。
func resolveBundleHead(hdr *BundleHeader, headTarget string) string {
	names := make([]string, 0, len(hdr.Refs))
	for _, r := range hdr.Refs {
		if strings.HasPrefix(r.Name, "refs/heads/") {
			names = append(names, r.Name)
		}
	}
	sort.Strings(names)
	if headTarget != "" {
		for _, n := range names {
			if n == headTarget {
				return n
			}
		}
	}
	if hdr.HeadOid.IsZero() {
		return ""
	}
	for _, r := range hdr.Refs {
		if strings.HasPrefix(r.Name, "refs/heads/") && r.Oid == hdr.HeadOid {
			return r.Name
		}
	}
	return ""
}

// safeRefName 拒绝可能越出仓库根的 ref 名（恢复路径用）。
func safeRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "\\") || strings.ContainsAny(name, "\x00\n\r") {
		return false
	}
	return true
}

// countingWriter 统计写出字节数。
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
