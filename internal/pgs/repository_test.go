package pgs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewBareRepoConfig(t *testing.T) {
	config := NewBareRepoConfig()
	t.Log(config.toString())
}

func TestInitBareCreatesPgitJSON(t *testing.T) {
	GitRoot = os.TempDir()
	repo, err := InitBare("test1", "this is test repo", "master")
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

func TestInitBareCustomDefaultBranch(t *testing.T) {
	GitRoot = os.TempDir()
	repo, err := InitBare("test-custom", "custom default branch", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Delete()

	defaultBranch, err := repo.DefaultBranch()
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if defaultBranch != "main" {
		t.Fatalf("defaultBranch = %q, want 'main'", defaultBranch)
	}

	// check HEAD file content
	headPath := filepath.Join(repo.Path(), "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	want := "ref: refs/heads/main\n"
	if string(data) != want {
		t.Fatalf("HEAD content = %q, want %q", string(data), want)
	}
}

func TestSetDefaultBranch(t *testing.T) {
	GitRoot = os.TempDir()
	repo, err := InitBare("test-set", "test set default", "master")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Delete()

	// create master and develop refs manually
	err = os.MkdirAll(filepath.Join(repo.Path(), "refs", "heads"), 0o777)
	if err != nil {
		t.Fatal(err)
	}
	oidA := "a5ccb972673562ef5bad1a6cced799f9d71a796b"
	writeRef := func(branch string) {
		p := filepath.Join(repo.Path(), "refs", "heads", branch)
		err := os.WriteFile(p, []byte(oidA+"\n"), 0o666)
		if err != nil {
			t.Fatal(err)
		}
	}
	writeRef("master")
	writeRef("develop")

	// initial default is master
	db, err := repo.DefaultBranch()
	if err != nil {
		t.Fatal(err)
	}
	if db != "master" {
		t.Fatalf("initial default = %q, want master", db)
	}

	// set to develop (valid, exists)
	err = repo.SetDefaultBranch("develop")
	if err != nil {
		t.Fatalf("SetDefaultBranch(develop): %v", err)
	}
	db, err = repo.DefaultBranch()
	if err != nil {
		t.Fatal(err)
	}
	if db != "develop" {
		t.Fatalf("default after set = %q, want develop", db)
	}

	// try set to non-existent branch should fail
	err = repo.SetDefaultBranch("nonexist")
	if err == nil {
		t.Fatalf("SetDefaultBranch(nonexist) should fail, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected 'does not exist' error, got: %v", err)
	}

	// invalid branch name should fail
	err = repo.SetDefaultBranch("../foo")
	if err == nil {
		t.Fatalf("SetDefaultBranch(../foo) should fail, got nil")
	}
}

func TestManagerScanAndAlias(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pgit-test-*")
	defer os.RemoveAll(dir)

	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	if err := ReposManager.CreateRepository("alpha", "alpha repo", "master"); err != nil {
		t.Fatal(err)
	}
	if err := ReposManager.CreateRepository("beta", "beta repo", "master"); err != nil {
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

	_ = ReposManager.CreateRepository("scanrepo", "", "master")
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

func TestCreateMirrorRepository(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pgit-mirror-*")
	defer os.RemoveAll(dir)
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	mirror := &MirrorConfig{
		RemoteURL:    "https://github.com/example/repo.git",
		SyncInterval: 300,
		AuthType:     "none",
	}
	if err := ReposManager.CreateMirrorRepository("mirror1", "test mirror", mirror); err != nil {
		t.Fatalf("CreateMirrorRepository: %v", err)
	}

	repo, err := ReposManager.GetRepository("mirror1")
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if !repo.IsMirror() {
		t.Fatal("IsMirror = false, want true")
	}
	if repo.Mirror.RemoteURL != "https://github.com/example/repo.git" {
		t.Errorf("RemoteURL = %q", repo.Mirror.RemoteURL)
	}
	if repo.Mirror.SyncInterval != 300 {
		t.Errorf("SyncInterval = %d, want 300", repo.Mirror.SyncInterval)
	}

	data, err := os.ReadFile(filepath.Join(repo.Path(), "pgit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\"mirror\"") {
		t.Error("pgit.json missing mirror field")
	}
}

func TestLoadRepo_MirrorBackwardCompat(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pgit-compat-*")
	defer os.RemoveAll(dir)
	GitRoot = dir

	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	ReposManager.CreateRepository("oldrepo", "old repo", "master")

	oldJSON := `{"name":"oldrepo","description":"old repo","aliases":["oldrepo"],"createdAt":"2026-01-01T00:00:00Z"}`
	os.WriteFile(filepath.Join(GitRoot, "oldrepo.git", "pgit.json"), []byte(oldJSON), 0o644)

	ReposManager = nil
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	repo, err := ReposManager.GetRepository("oldrepo")
	if err != nil {
		t.Fatal(err)
	}
	if repo.IsMirror() {
		t.Fatal("IsMirror = true, want false for old repo without mirror field")
	}
	if repo.Mirror != nil {
		t.Fatal("Mirror should be nil for old repo")
	}
}

func TestCreateMirrorRepository_Validation(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pgit-mirror-val-*")
	defer os.RemoveAll(dir)
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	err := ReposManager.CreateMirrorRepository("m1", "", &MirrorConfig{RemoteURL: ""})
	if err == nil {
		t.Fatal("empty RemoteURL should fail")
	}

	err = ReposManager.CreateMirrorRepository("m2", "", &MirrorConfig{RemoteURL: "ssh://example.com/repo.git"})
	if err == nil {
		t.Fatal("non-HTTP RemoteURL should fail")
	}

	err = ReposManager.CreateMirrorRepository("m3", "", &MirrorConfig{RemoteURL: "https://example.com/repo.git", SyncInterval: -1})
	if err == nil {
		t.Fatal("negative SyncInterval should fail")
	}
}
