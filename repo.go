package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repository struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	UpdateAt    uint32 `json:updateAt`
}

type Ref struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Creator string `json:"creator"`
	Subject string `json:"subject"`
}

type TreeNode struct {
	Type string `json:"type"`
	Hash string `json:"hash"`
	Name string `json:"name"`
}

func (repo Repository) InitBare() error {
	repopath := RepositoryDir(repo.Name)
	gitInitCmd := exec.Command("git", "init", "--bare", repopath)
	_, err := gitInitCmd.CombinedOutput()
	if err != nil {
		return err
	}

	desc := fmt.Sprintf("%s;%s", repo.Name, repo.Description)
	err = ioutil.WriteFile(
		filepath.Join(repopath, "description"), []byte(desc), os.ModePerm)

	return err
}

func (repo Repository) Delete() error {
	repopath := RepositoryDir(repo.Name)
	err := os.RemoveAll(repopath)
	return err
}

func (repo Repository) UpdateRepository() error {
	return nil
}

func (repo Repository) Tree(tree_ish string, subtree string) ([]TreeNode, error) {
	repopath := RepositoryDir(repo.Name)
	tree := make([]TreeNode, 0)
	var cmd *exec.Cmd
	if len(subtree) > 0 {
		cmd = exec.Command("git", "ls-tree", tree_ish, subtree)
	} else {
		cmd = exec.Command("git", "ls-tree", tree_ish)
	}

	cmd.Dir = repopath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	raw := strings.Trim(string(output), "\n ")
	files := strings.Split(raw, "\n")

	//100644 blob 2bb65d4c4017c1b1fec26ea46bb6e740d343ba7a\tREADME.md
	for _, row := range files {
		if len(row) == 0 {
			continue
		}
		if len(row) < 53 {
			return nil, fmt.Errorf("Read tree failed '%s'", row)
		}

		index := 53 + len(subtree)
		// log.Printf("index %d subtree %s row %s", index, subtree, row)
		if index > len(row) {
			return nil, fmt.Errorf("Read tree failed '%s'", row)
		}

		tree = append(tree, TreeNode{
			Type: row[7:11],
			Hash: row[12:52],
			Name: row[index:],
		})
	}

	return tree, nil
}

func (repo Repository) Blob(tree_ish string, path string) (io.ReadCloser, error) {
	repopath := RepositoryDir(repo.Name)
	cmd := exec.Command("git", "ls-tree", tree_ish, path)
	cmd.Dir = repopath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	raw := strings.Trim(string(output), "\n ")

	if len(raw) < 53 || raw[7:11] != "blob" {
		return nil, fmt.Errorf("Not blob file")
	}
	return repo.Object(raw[12:52])
}

func (repo Repository) Object(hash string) (io.ReadCloser, error) {
	repopath := RepositoryDir(repo.Name)
	cmd := exec.Command(
		"git", "cat-file", "blob", hash)
	cmd.Dir = repopath
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Start()
	return output, nil
}

func (repo Repository) Archive(tree_ish string) (io.ReadCloser, error) {
	repopath := RepositoryDir(repo.Name)
	cmd := exec.Command(
		"git", "archive", "--format=zip",
		fmt.Sprintf("--prefix=%s/", repo.Name),
		tree_ish)
	cmd.Dir = repopath
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Start()
	return output, nil
}

func (repo Repository) ForEachRef() ([]*Ref, error) {
	repopath := RepositoryDir(repo.Name)
	// git for-each-ref --format="%(objecttype) %(refname:short) %(creator) %(contents:subject)"
	cmd := exec.Command("git", "for-each-ref", "--format=%(objecttype)%07%(refname:short)%07%(creator)%07%(contents:subject)")
	cmd.Dir = repopath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	raw := strings.Trim(string(output), "\n ")

	refs := make([]*Ref, 0)
	for _, row := range strings.Split(raw, "\n") {
		r := strings.Split(row, fmt.Sprintf("%c", 0x07))
		log.Printf("%v", r)
		refs = append(refs, &Ref{
			Type:    r[0],
			Name:    r[1],
			Creator: r[2],
			Subject: r[3],
		})
	}
	return refs, nil
}

func CheckRepository(repoDir string) (*Repository, error) {
	if !IsRepositoryDir(repoDir) {
		return nil, fmt.Errorf("%s Not Repository directory", repoDir)
	}
	raw, err := ioutil.ReadFile(filepath.Join(GetSettings().GitRoot, repoDir, "description"))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(repoDir, ".git")
	description := strings.TrimPrefix(string(raw), fmt.Sprintf("%s;", name))

	// load metadata
	repo := &Repository{
		Name:        name,
		Description: description,
	}
	return repo, nil
}

func RepositoryDir(name string) string {
	return filepath.Join(GetSettings().GitRoot, fmt.Sprintf("%s.git", name))
}

func IsRepositoryDir(name string) bool {
	if !strings.HasSuffix(name, ".git") {
		return false
	}
	_, err := os.Stat(filepath.Join(GetSettings().GitRoot, name, "description"))
	if os.IsNotExist(err) {
		return false
	}

	return true
}
