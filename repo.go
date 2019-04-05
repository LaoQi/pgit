package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repository struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Branch      []string `json:"branch"`
	UpdateAt    uint32   `json:updateAt`
}

type TreeNode struct {
	Type string `json:"type"`
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

func (repo Repository) Tree(branch string, subtree string) ([]TreeNode, error) {
	repopath := RepositoryDir(repo.Name)
	tree := make([]TreeNode, 0)
	cmd := exec.Command("git", "ls-tree", branch)
	cmd.Dir = repopath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	raw := strings.Trim(string(output), "\n ")
	files := strings.Split(raw, "\n")

	for _, row := range files {
		if len(row) < 53 {
			return nil, fmt.Errorf("Read tree error '%s'", row)
		}
		tree = append(tree, TreeNode{
			Type: row[7:11],
			Name: row[53:],
		})
	}

	return tree, nil
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
