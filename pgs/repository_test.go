package pgs

import (
	"os"
	"testing"
)

func TestNewBareRepoConfig(t *testing.T) {
	config := NewBareRepoConfig()
	t.Log(config.toString())
}

func TestInitBare(t *testing.T) {
	GitRoot = os.TempDir()
	repo, err := InitBare("test1", "this is test repo")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(repo.Path())
	_ = repo.Delete()
}
