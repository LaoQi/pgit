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
func InitBare(name string, description string) (*Repository, error) {
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
	_ = os.WriteFile(filepath.Join(root, "HEAD"), []byte("ref: refs/heads/master\n"), os.ModePerm)
	_ = os.WriteFile(filepath.Join(root, "info", "exclude"), []byte("# Auto generated\n# Lines that start with '#' are comments.\n"), os.ModePerm)

	if err := repo.SaveMetadata(); err != nil {
		return nil, err
	}
	return repo, nil
}

func (repo Repository) Delete() error {
	return os.RemoveAll(repo.Path())
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
