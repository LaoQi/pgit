package pgs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	tree := make([]TreeNode, 0)
	var cmd *exec.Cmd
	if len(subtree) > 0 {
		cmd = exec.Command("git", "ls-tree", treeIsh, subtree)
	} else {
		cmd = exec.Command("git", "ls-tree", treeIsh)
	}

	cmd.Dir = repo.Path()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	raw := strings.Trim(string(output), "\n ")
	files := strings.Split(raw, "\n")

	for _, row := range files {
		if len(row) == 0 {
			continue
		}
		if len(row) < 53 {
			return nil, fmt.Errorf("read tree failed '%s'", row)
		}

		index := 53 + len(subtree)
		if index > len(row) {
			return nil, fmt.Errorf("read tree failed '%s'", row)
		}

		tree = append(tree, TreeNode{
			Type: row[7:11],
			Hash: row[12:52],
			Name: row[index:],
		})
	}

	return tree, nil
}

func (repo Repository) Blob(ref string, path string) (io.ReadCloser, error) {
	cmd := exec.Command("git", "cat-file", "blob", fmt.Sprintf("%s:%s", ref, path))
	cmd.Dir = repo.Path()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	_ = cmd.Start()
	return output, nil
}

func (repo Repository) Archive(ref string) (io.ReadCloser, error) {
	cmd := exec.Command("git", "archive", "--format=zip",
		fmt.Sprintf("--prefix=%s/", repo.Name), ref)
	cmd.Dir = repo.Path()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	_ = cmd.Start()
	return output, nil
}

func (repo Repository) ForEachRef() ([]*Ref, error) {
	cmd := exec.Command("git", "for-each-ref",
		"--format=%(objecttype)%07%(refname:short)%07%(creator)%07%(contents:subject)")
	cmd.Dir = repo.Path()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	raw := strings.Trim(string(output), "\n ")

	refs := make([]*Ref, 0)
	for _, row := range strings.Split(raw, "\n") {
		r := strings.Split(row, fmt.Sprintf("%c", 0x07))
		if len(r) != 4 {
			continue
		}
		ref := &Ref{
			Type:    r[0],
			Name:    r[1],
			Subject: r[3],
		}

		creator := strings.Split(r[2], " ")
		timestamp, _ := strconv.ParseUint(creator[2], 10, 0)
		if len(creator) > 2 {
			ref.Author = creator[0]
			ref.Email = creator[1]
			ref.Timestamp = timestamp
		}
		refs = append(refs, ref)
	}
	return refs, nil
}
