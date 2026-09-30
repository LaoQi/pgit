package pgs

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pgit/internal/pgs/git"
)

var GitRoot string

// MirrorConfig 描述仓库的远端配置，被两种模式共用：
//
//   - 镜像仓库（Mode 为空或 "pull"，默认）：按 SyncInterval 从上游拉取，禁止 push
//   - 中转仓库（Mode == "relay"）：以上游为基线，允许下游快进推送并转发到上游
//
// 两种模式共用 RemoteURL/Auth/Proxy/LastSync/LastError；relay 额外使用
// Refs/AllowDelete/LastPush/LastPushError/PendingRefs。
type MirrorConfig struct {
	RemoteURL    string    `json:"remoteUrl"`
	SyncInterval int       `json:"syncInterval"`
	AuthType     string    `json:"authType"`
	Username     string    `json:"username,omitempty"`
	Password     string    `json:"password,omitempty"`
	Proxy        string    `json:"proxy,omitempty"`
	LastSync     time.Time `json:"lastSync,omitempty"`
	LastError    string    `json:"lastError,omitempty"`

	// Mode 见类型注释；缺省（空）等价 "pull"，老元数据因此无需迁移。
	Mode string `json:"mode,omitempty"`

	// --- 以下仅中转仓库（relay）使用 ---

	// Refs 是准入与转发的 ref 前缀白名单，空表示默认（refs/heads/ + refs/tags/）。
	Refs []string `json:"refs,omitempty"`
	// AllowDelete 控制是否允许把下游的 ref 删除转发到上游，nil 表示允许（默认）。
	AllowDelete *bool `json:"allowDelete,omitempty"`
	// LastPush/LastPushError 是最近一次转发上游的结果（与拉取侧的 LastSync/LastError 分开）。
	LastPush      time.Time `json:"lastPush,omitempty"`
	LastPushError string    `json:"lastPushError,omitempty"`
	// PendingRefs 是已通过准入但尚未成功转发到上游的 ref，落盘以便重启后补推。
	PendingRefs []string `json:"pendingRefs,omitempty"`
}

type Repository struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Aliases     []string      `json:"aliases"`
	CreatedAt   time.Time     `json:"createdAt"`
	Mirror      *MirrorConfig `json:"mirror,omitempty"`

	// root 是该仓库所属的存储根目录（<root>/<name>.git），不参与 JSON 序列化。
	// 由 RepositoriesManager/InitBare 注入，使 Repository 自包含、Path() 不再依赖全局状态。
	root string
}

func (repo *Repository) IsMirror() bool {
	return repo.Mirror != nil
}

// Snapshot 返回仓库元数据的深拷贝。调用方拿到的是与内部状态解耦的不可变值，
// 可安全读取、序列化或跨 goroutine 传递。
func (repo *Repository) Snapshot() *Repository {
	if repo == nil {
		return nil
	}
	c := *repo
	if repo.Aliases != nil {
		c.Aliases = append([]string(nil), repo.Aliases...)
	}
	if repo.Mirror != nil {
		m := *repo.Mirror
		if repo.Mirror.Refs != nil {
			m.Refs = append([]string(nil), repo.Mirror.Refs...)
		}
		if repo.Mirror.PendingRefs != nil {
			m.PendingRefs = append([]string(nil), repo.Mirror.PendingRefs...)
		}
		if repo.Mirror.AllowDelete != nil {
			v := *repo.Mirror.AllowDelete
			m.AllowDelete = &v
		}
		c.Mirror = &m
	}
	return &c
}

// Root 返回仓库所属的存储根目录。
func (repo *Repository) Root() string {
	if repo.root != "" {
		return repo.root
	}
	return GitRoot // 过渡兜底：未注入 root 的仓库对象（阶段 2-3 收口）
}

func (repo *Repository) Path() string {
	return filepath.Join(repo.Root(), fmt.Sprintf("%s.git", repo.Name))
}

func (repo *Repository) SaveMetadata() error {
	data, err := json.MarshalIndent(repo, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(repo.Path(), "pgit.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (repo *Repository) HasAlias(alias string) bool {
	for _, a := range repo.Aliases {
		if a == alias {
			return true
		}
	}
	return false
}

type Ref struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Author    string `json:"author"`
	Email     string `json:"email"`
	Timestamp uint64 `json:"timestamp"`
	Subject   string `json:"subject"`
}

type TreeNode struct {
	Type string `json:"type"`
	Hash string `json:"hash"`
	Name string `json:"name"`
}

type Commit struct {
	Hash      string   `json:"hash"`
	Parents   []string `json:"parents"`
	Author    string   `json:"author"`
	Email     string   `json:"email"`
	Timestamp uint64   `json:"timestamp"`
	Subject   string   `json:"subject"`
}

type RepoConfigSection struct {
	Items map[string]string
}

type RepoConfig struct {
	Sections map[string]RepoConfigSection
}

func (rc *RepoConfig) toString() string {
	var lines []string
	for title, section := range rc.Sections {
		lines = append(lines, fmt.Sprintf("[%s]", title))
		for k, v := range section.Items {
			lines = append(lines, fmt.Sprintf("\t%s = %s", k, v))
		}
	}
	return strings.Join(lines, "\n")
}

func NewBareRepoConfig() *RepoConfig {
	return &RepoConfig{
		Sections: map[string]RepoConfigSection{
			"core": {Items: map[string]string{
				"repositoryformatversion": "0",
				"filemode":                "true",
				"bare":                    "true",
			}},
		},
	}
}

// InitBare builds a bare repository directory by hand (no `git init --bare`),
// writes config/HEAD/description plus pgit.json metadata.
// gitRoot 指定存储根目录（<gitRoot>/<name>.git）；defaultBranch 设置 HEAD symref
// 目标（如 "master"），空值默认 "master"。
func InitBare(gitRoot string, name string, description string, defaultBranch string) (*Repository, error) {
	if defaultBranch == "" {
		defaultBranch = "master"
	}
	repo := &Repository{
		Name:        name,
		Description: description,
		Aliases:     []string{name},
		CreatedAt:   time.Now(),
		root:        gitRoot,
	}
	desc := fmt.Sprintf("%s;%s", name, description)
	root := repo.Path()
	config := NewBareRepoConfig().toString()

	// 创建失败（任一步骤出错）时回滚已建目录，避免残留半成品仓库被扫描静默跳过。
	rollback := func() { _ = os.RemoveAll(root) }

	if err := os.Mkdir(root, 0o750); err != nil {
		return nil, err
	}
	for _, sub := range []string{
		"branches", "hooks", "info", "objects/info", "objects/pack", "refs/heads", "refs/tags",
	} {
		paths := append([]string{root}, strings.Split(sub, "/")...)
		if err := os.MkdirAll(filepath.Join(paths...), 0o750); err != nil {
			rollback()
			return nil, err
		}
	}

	files := map[string]string{
		"description":                    desc,
		"config":                         config,
		"HEAD":                           fmt.Sprintf("ref: refs/heads/%s\n", defaultBranch),
		filepath.Join("info", "exclude"): "# Auto generated\n# Lines that start with '#' are comments.\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o640); err != nil {
			rollback()
			return nil, err
		}
	}

	if err := repo.SaveMetadata(); err != nil {
		rollback()
		return nil, err
	}
	return repo, nil
}

func (repo Repository) Delete() error {
	git.InvalidateRefsCache(repo.Path())
	return os.RemoveAll(repo.Path())
}

// deletedMarkerFile 软删除标记文件：出现在仓库目录内（与 pgit.json 并列）时，
// 启动扫描跳过该仓库——不进索引、不参与 ref 冲突检测、不注册同步，git 传输与
// 管理 API 均不可见。数据全部保留在磁盘；删除该标记文件并重启进程即可恢复仓库。
const deletedMarkerFile = "pgit.deleted"

// MarkDeleted 写入软删除标记（内容为删除时刻的 RFC3339 时间戳），不触碰任何 git 数据。
func (repo Repository) MarkDeleted() error {
	content := []byte(time.Now().UTC().Format(time.RFC3339) + "\n")
	return os.WriteFile(filepath.Join(repo.Path(), deletedMarkerFile), content, 0o644)
}

// DefaultBranch 返回仓库默认分支的 short 名（如 "master"）。
// 解析 HEAD symref 目标，去掉 refs/heads/ 前缀。detached HEAD 或 HEAD 缺失返回空字符串。
func (repo Repository) DefaultBranch() (string, error) {
	rs := git.NewRefStore(repo.Path())
	target, err := rs.Head()
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if target == "" {
		return "", nil // detached
	}
	return strings.TrimPrefix(target, "refs/heads/"), nil
}

// SetDefaultBranch 将仓库默认分支切换为 branch（short 名，如 "develop"）。
// 要求该分支必须已存在（refs/heads/<branch> 存在），否则返回错误。
func (repo Repository) SetDefaultBranch(branch string) error {
	if err := ValidateDefaultBranch(branch); err != nil {
		return err
	}
	fullName := "refs/heads/" + branch
	rs := git.NewRefStore(repo.Path())
	if _, err := rs.Get(fullName); err != nil {
		return fmt.Errorf("branch %q does not exist", branch)
	}
	return rs.SetHead(fullName)
}

// ValidateDefaultBranch 校验默认分支名合法性。
//
// 分支名来自 git 客户端（可能含 @、+ 等别名规则之外的字符），因此这里保持宽松规则，
// 不复用已收紧的 ref（仓库名/别名）规则。
func ValidateDefaultBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("branch is empty")
	}
	if strings.HasPrefix(branch, "/") {
		return fmt.Errorf("branch must not start with '/'")
	}
	if strings.HasSuffix(branch, "/") {
		return fmt.Errorf("branch must not end with '/'")
	}
	if strings.Contains(branch, "//") {
		return fmt.Errorf("branch must not contain empty segment")
	}
	if strings.Contains(branch, "..") {
		return fmt.Errorf("branch must not contain '..'")
	}
	return nil
}

func (repo Repository) Tree(treeIsh string, subtree string) ([]TreeNode, error) {
	_, treeOid, err := git.ResolveTreeIsh(repo.Path(), treeIsh)
	if err != nil {
		return nil, err
	}
	store := git.NewObjectStore(repo.Path())
	entries, err := git.TreeAt(store, treeOid, subtree)
	if err != nil {
		return nil, err
	}
	nodes := make([]TreeNode, 0, len(entries))
	for _, e := range entries {
		nodes = append(nodes, TreeNode{
			Type: modeType(e.Mode),
			Hash: string(e.Oid),
			Name: e.Name,
		})
	}
	return nodes, nil
}

func (repo Repository) Blob(ref string, path string) (io.ReadCloser, error) {
	_, treeOid, err := git.ResolveTreeIsh(repo.Path(), ref)
	if err != nil {
		return nil, err
	}
	store := git.NewObjectStore(repo.Path())
	blob, err := git.BlobAt(store, treeOid, path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(blob.Content)), nil
}

func (repo Repository) Archive(ref string) (io.ReadCloser, error) {
	root := repo.Path()
	commitOid, treeOid, err := git.ResolveTreeIsh(root, ref)
	if err != nil {
		return nil, err
	}
	store := git.NewObjectStore(root)
	var modTime time.Time
	if commitOid != "" {
		if obj, err := store.Read(commitOid); err == nil {
			if c, err := git.ParseCommit(obj.Content); err == nil {
				modTime = c.Committer.Time()
			}
		}
	}
	pr, pw := io.Pipe()
	go func() {
		err := archiveZip(pw, store, treeOid, repo.Name, modTime)
		pw.CloseWithError(err)
	}()
	return pr, nil
}

func (repo Repository) ForEachRef() ([]*Ref, error) {
	infos, err := git.ForEachRefs(repo.Path())
	if err != nil {
		return nil, err
	}
	refs := make([]*Ref, 0, len(infos))
	for _, info := range infos {
		refs = append(refs, &Ref{
			Type:      info.Type,
			Name:      info.Name,
			Author:    info.Author,
			Email:     info.Email,
			Timestamp: uint64(info.Timestamp),
			Subject:   info.Subject,
		})
	}
	return refs, nil
}

// LastCommitTime 返回仓库最后提交时间：所有 ref 最终指向的 commit 的 committer
// 时间最大值（annotated tag 引用先逐层 peel 到 commit，取 commit 时间而非 tagger
// 时间）。遍历经 git.ForEachRefs 的 refs 指纹缓存，refs 未变时不会重复读取对象。
// 仓库没有任何提交（空仓库）时返回零值。
func (repo Repository) LastCommitTime() time.Time {
	infos, err := git.ForEachRefs(repo.Path())
	if err != nil {
		return time.Time{}
	}
	store := git.NewObjectStore(repo.Path())
	var latest time.Time
	for _, info := range infos {
		ts := time.Unix(info.Timestamp, 0)
		if info.Type == string(git.ObjTag) {
			ts = peelCommitTime(store, info.Oid)
		}
		if ts.IsZero() || (!latest.IsZero() && !ts.After(latest)) {
			continue
		}
		latest = ts
	}
	return latest
}

// peelCommitTime 从任意对象出发逐层 peel 到 commit 并返回 committer 时间
// （深度上限与 git 包 derefToTree 一致）；非 commit/tag 或解析失败返回零值。
func peelCommitTime(store git.ObjectStore, oid git.Oid) time.Time {
	const maxDepth = 16
	cur := oid
	for depth := 0; depth < maxDepth; depth++ {
		obj, err := store.Read(cur)
		if err != nil {
			return time.Time{}
		}
		switch obj.Type {
		case git.ObjCommit:
			c, err := git.ParseCommit(obj.Content)
			if err != nil {
				return time.Time{}
			}
			return c.Committer.Time()
		case git.ObjTag:
			tg, err := git.ParseTag(obj.Content)
			if err != nil {
				return time.Time{}
			}
			cur = tg.Object
			continue
		default:
			return time.Time{}
		}
	}
	return time.Time{}
}
func (repo Repository) Commits(ref string, limit int) ([]Commit, error) {
	commitOid, _, err := git.ResolveTreeIsh(repo.Path(), ref)
	if err != nil {
		return nil, err
	}
	if commitOid == "" {
		return nil, nil
	}
	store := git.NewObjectStore(repo.Path())
	log, err := git.CommitLog(store, commitOid, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Commit, 0, len(log))
	for _, ci := range log {
		parents := make([]string, 0, len(ci.Parents))
		for _, p := range ci.Parents {
			parents = append(parents, string(p))
		}
		out = append(out, Commit{
			Hash:      string(ci.Oid),
			Parents:   parents,
			Author:    ci.Author.Name,
			Email:     ci.Author.Email,
			Timestamp: uint64(ci.Author.Timestamp),
			Subject:   ci.Subject,
		})
	}
	return out, nil
}

// modeType 将 tree entry 的 mode 映射为节点类型字符串，与 git ls-tree 输出一致。
func modeType(mode uint32) string {
	switch {
	case mode == 0o040000:
		return "tree"
	case mode == 0o160000:
		return "commit"
	default:
		return "blob"
	}
}

// archiveZip 递归遍历 treeOid，将所有 blob 写入 zip，路径前缀为 prefix。
// gitlink（0o160000）跳过，与 git archive 行为一致。
func archiveZip(w io.Writer, store git.ObjectStore, treeOid git.Oid, prefix string, modTime time.Time) error {
	zw := zip.NewWriter(w)
	if err := walkTreeToZip(store, treeOid, prefix, zw, modTime); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

func walkTreeToZip(store git.ObjectStore, treeOid git.Oid, prefix string, zw *zip.Writer, modTime time.Time) error {
	entries, err := git.TreeAt(store, treeOid, "")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Mode == 0o160000 {
			continue
		}
		path := prefix + "/" + e.Name
		if e.Mode == 0o040000 {
			if err := walkTreeToZip(store, e.Oid, path, zw, modTime); err != nil {
				return err
			}
			continue
		}
		blob, err := store.Read(e.Oid)
		if err != nil {
			return err
		}
		fh := &zip.FileHeader{
			Name:     path,
			Method:   zip.Deflate,
			Modified: modTime,
		}
		fw, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		if _, err := fw.Write(blob.Content); err != nil {
			return err
		}
	}
	return nil
}
