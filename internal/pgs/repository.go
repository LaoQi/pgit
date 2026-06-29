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

type Repository struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Aliases     []string  `json:"aliases"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (repo *Repository) Path() string {
	return filepath.Join(GitRoot, fmt.Sprintf("%s.git", repo.Name))
}

func (repo *Repository) SaveMetadata() error {
	data, err := json.MarshalIndent(repo, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(repo.Path(), "pgit.json"), data, os.ModePerm)
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
	Hash      string `json:"hash"`
	Parents   []string `json:"parents"`
	Author    string `json:"author"`
	Email     string `json:"email"`
	Timestamp uint64 `json:"timestamp"`
	Subject   string `json:"subject"`
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
// defaultBranch sets the initial HEAD symref target (e.g. "master"); empty defaults to "master".
func InitBare(name string, description string, defaultBranch string) (*Repository, error) {
	if defaultBranch == "" {
		defaultBranch = "master"
	}
	repo := &Repository{
		Name:        name,
		Description: description,
		Aliases:     []string{name},
		CreatedAt:   time.Now(),
	}
	desc := fmt.Sprintf("%s;%s", name, description)
	root := repo.Path()
	config := NewBareRepoConfig().toString()

	err := os.Mkdir(root, os.ModePerm)
	if err != nil {
		return nil, err
	}

	for _, sub := range []string{
		"branches", "hooks", "info", "objects/info", "objects/pack", "refs/heads", "refs/tags",
	} {
		paths := append([]string{root}, strings.Split(sub, "/")...)
		err := os.MkdirAll(filepath.Join(paths...), os.ModePerm)
		if err != nil {
			return nil, err
		}
	}

	_ = os.WriteFile(filepath.Join(root, "description"), []byte(desc), os.ModePerm)
	_ = os.WriteFile(filepath.Join(root, "config"), []byte(config), os.ModePerm)
	_ = os.WriteFile(filepath.Join(root, "HEAD"), []byte(fmt.Sprintf("ref: refs/heads/%s\n", defaultBranch)), os.ModePerm)
	_ = os.WriteFile(filepath.Join(root, "info", "exclude"), []byte("# Auto generated\n# Lines that start with '#' are comments.\n"), os.ModePerm)

	if err := repo.SaveMetadata(); err != nil {
		return nil, err
	}
	return repo, nil
}

func (repo Repository) Delete() error {
	return os.RemoveAll(repo.Path())
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

// ValidateDefaultBranch 校验默认分支名合法性（与 alias 校验规则一致）。
func ValidateDefaultBranch(branch string) error {
	return ValidateAlias(branch)
}

func (repo Repository) Tree(treeIsh string, subtree string) ([]TreeNode, error) {
	_, treeOid, err := git.ResolveTreeIsh(repo.Path(), treeIsh)
	if err != nil {
		return nil, err
	}
	store := &git.LooseStore{Root: filepath.Join(repo.Path(), "objects")}
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
	store := &git.LooseStore{Root: filepath.Join(repo.Path(), "objects")}
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
	store := &git.LooseStore{Root: filepath.Join(root, "objects")}
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

func (repo Repository) Commits(ref string, limit int) ([]Commit, error) {
	commitOid, _, err := git.ResolveTreeIsh(repo.Path(), ref)
	if err != nil {
		return nil, err
	}
	if commitOid == "" {
		return nil, nil
	}
	store := &git.LooseStore{Root: filepath.Join(repo.Path(), "objects")}
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
func archiveZip(w io.Writer, store *git.LooseStore, treeOid git.Oid, prefix string, modTime time.Time) error {
	zw := zip.NewWriter(w)
	if err := walkTreeToZip(store, treeOid, prefix, zw, modTime); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

func walkTreeToZip(store *git.LooseStore, treeOid git.Oid, prefix string, zw *zip.Writer, modTime time.Time) error {
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
