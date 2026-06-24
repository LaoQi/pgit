package pgs

import (
	"os"
	"testing"
	"time"
)

func TestNewBareRepoConfig(t *testing.T) {
	config := NewBareRepoConfig()
	t.Log(config.toString())
}

func TestInitBareCreatesPgitJSON(t *testing.T) {
	GitRoot = os.TempDir()
	repo, err := InitBare("test1", "this is test repo")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(repo.Path())

	if !FileExist(repo.Path()) {
		t.Fatalf("repo dir not created at %s", repo.Path())
	}
	metaPath := repo.Path() + string(os.PathSeparator) + "pgit.json"
	if !FileExist(metaPath) {
		t.Fatalf("pgit.json not created at %s", metaPath)
	}
	if len(repo.Aliases) != 1 || repo.Aliases[0] != "test1" {
		t.Fatalf("expected default alias [test1], got %v", repo.Aliases)
	}
	if repo.CreatedAt.IsZero() {
		t.Fatalf("createdAt not set")
	}

	_ = repo.Delete()
}

func TestManagerScanAndAlias(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pgit-test-*")
	defer os.RemoveAll(dir)

	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	if err := ReposManager.CreateRepository("alpha", "alpha repo"); err != nil {
		t.Fatal(err)
	}
	if err := ReposManager.CreateRepository("beta", "beta repo"); err != nil {
		t.Fatal(err)
	}

	if len(ReposManager.List()) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(ReposManager.List()))
	}

	if err := ReposManager.AddAlias("alpha", "team/alpha"); err != nil {
		t.Fatal(err)
	}
	repo, err := ReposManager.GetByAlias("team/alpha")
	if err != nil {
		t.Fatalf("alias lookup failed: %v", err)
	}
	if repo.Name != "alpha" {
		t.Fatalf("alias resolved to wrong repo: %s", repo.Name)
	}

	if err := ReposManager.RemoveAlias("alpha", "team/alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReposManager.GetByAlias("team/alpha"); err == nil {
		t.Fatal("expected alias lookup to fail after removal")
	}

	if err := ReposManager.RemoveAlias("alpha", "alpha"); err == nil {
		t.Fatal("should reject removing default alias (name)")
	}
}

func TestManagerScanRestoresAliases(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pgit-test-*")
	defer os.RemoveAll(dir)

	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	_ = ReposManager.CreateRepository("scanrepo", "")
	_ = ReposManager.AddAlias("scanrepo", "alias1")
	_ = ReposManager.AddAlias("scanrepo", "alias2")

	ReposManager = nil
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})

	repo, err := ReposManager.GetRepository("scanrepo")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.HasAlias("alias1") || !repo.HasAlias("alias2") || !repo.HasAlias("scanrepo") {
		t.Fatalf("aliases not restored on rescan: %v", repo.Aliases)
	}
}

func TestValidateRepoName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"foo", true},
		{"", false},
		{"foo/bar", false},
		{"..", false},
		{"foo/../bar", false},
		{".hidden", false},
		{"api", false},
	}
	for _, c := range cases {
		err := ValidateRepoName(c.name)
		if (err == nil) != c.ok {
			t.Errorf("ValidateRepoName(%q) ok=%v, want %v: %v", c.name, err == nil, c.ok, err)
		}
	}
}

func TestValidateAlias(t *testing.T) {
	cases := []struct {
		alias string
		ok    bool
	}{
		{"foo", true},
		{"team/foo", true},
		{"", false},
		{"/foo", false},
		{"foo/", false},
		{"foo//bar", false},
		{"foo/../bar", false},
		{"api", false},
		{"api/foo", false},
	}
	for _, c := range cases {
		err := ValidateAlias(c.alias)
		if (err == nil) != c.ok {
			t.Errorf("ValidateAlias(%q) ok=%v, want %v: %v", c.alias, err == nil, c.ok, err)
		}
	}
}

// ensure time import used
var _ = time.Now
