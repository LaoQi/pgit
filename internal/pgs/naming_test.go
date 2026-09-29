package pgs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 辅助 ----------

func newNamingManager(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { ReposManager = nil })
	return dir
}

// rewriteAliases 直接改写磁盘上的 pgit.json，用于构造扫描期冲突。
func rewriteAliases(t *testing.T, dir, repoName string, aliases []string) {
	t.Helper()
	path := filepath.Join(dir, repoName+".git", "pgit.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	meta["aliases"] = aliases
	out, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, out, 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustCreate(t *testing.T, name string) {
	t.Helper()
	if err := ReposManager.CreateRepository(name, "", "master"); err != nil {
		t.Fatalf("CreateRepository(%q): %v", name, err)
	}
}

func mustAddAlias(t *testing.T, name, alias string) {
	t.Helper()
	if err := ReposManager.AddAlias(name, alias); err != nil {
		t.Fatalf("AddAlias(%q, %q): %v", name, alias, err)
	}
}

// ---------- 规则（白名单 / 长度 / 保留字） ----------

func TestRefValidationRules(t *testing.T) {
	validNames := []string{"r1", "LaoQi_dotfile", "a-b.c", "x1_", "_x", "a..b", "LaoQi_install-pacman-for-gitbash"}
	for _, n := range validNames {
		if err := ValidateRepoName(n); err != nil {
			t.Errorf("ValidateRepoName(%q) = %v, want nil", n, err)
		}
	}

	invalidNames := []string{
		"", "a/b", ".hidden", "-x", "x-", "x.", "a b", "a%b", "a#b", "a?b", "a&b", "a+b",
		"a\tb", "café", "a\\b", "a:b", "a@b", "a;b", "a=b", "a~b", "a*b", "a'b",
		"api", "API", "healthz", "metrics", "__webui", "x.git", "x.GIT",
		strings.Repeat("a", refMaxLen+1), strings.Repeat("a", refSegmentMaxLen+1),
	}
	for _, n := range invalidNames {
		if err := ValidateRepoName(n); err == nil {
			t.Errorf("ValidateRepoName(%q) = nil, want error", n)
		}
	}

	validAliases := []string{"owner/repo", "a/b/c", "team_1/x-2", "healthz/x", "api-not-reserved"}
	for _, a := range validAliases {
		if err := ValidateAlias(a); err != nil {
			t.Errorf("ValidateAlias(%q) = %v, want nil", a, err)
		}
	}

	invalidAliases := []string{
		"", "/a", "a/", "a//b", "a/./b", "a/../b", "a/.b", "a/b.", "a/-b", "a/b-",
		"a/b.git", "api/v1", "api", "__webui", "__webui/x", "healthz", "metrics",
		"a/ b", "a/%b", "a/ü",
		strings.Join([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, "/"), // 9 段
	}
	for _, a := range invalidAliases {
		if err := ValidateAlias(a); err == nil {
			t.Errorf("ValidateAlias(%q) = nil, want error", a)
		}
	}
}

// 分支名校验与 ref 规则解耦：别名规则之外的字符仍可用于分支名。
func TestBranchNameValidationStaysLoose(t *testing.T) {
	for _, b := range []string{"master", "feature/x", "release-1.0", "user@host", "v1.0+hotfix"} {
		if err := ValidateDefaultBranch(b); err != nil {
			t.Errorf("ValidateDefaultBranch(%q) = %v, want nil", b, err)
		}
	}
	for _, b := range []string{"", "/x", "x/", "a//b", "a..b"} {
		if err := ValidateDefaultBranch(b); err == nil {
			t.Errorf("ValidateDefaultBranch(%q) = nil, want error", b)
		}
	}
}

// ---------- 唯一性：建仓不得抢走既有 alias ----------

func TestCreateRepositoryRejectsExistingAlias(t *testing.T) {
	newNamingManager(t)
	mustCreate(t, "alpha")
	mustAddAlias(t, "alpha", "gamma")

	err := ReposManager.CreateRepository("gamma", "", "master")
	if !errors.Is(err, ErrRefConflict) {
		t.Fatalf("create with taken alias = %v, want ErrRefConflict", err)
	}

	// 既有仓库仍按别名可达，未被劫持
	repo, err := ReposManager.Resolve("gamma")
	if err != nil {
		t.Fatalf("Resolve(gamma): %v", err)
	}
	if repo.Name != "alpha" {
		t.Fatalf("alias gamma resolved to %q, want alpha", repo.Name)
	}
	if _, err := ReposManager.GetRepository("gamma"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("GetRepository(gamma) = %v, want ErrRepoNotFound", err)
	}
}

func TestCreateRepositoryRejectsCaseVariantRef(t *testing.T) {
	newNamingManager(t)
	mustCreate(t, "Foo")
	if err := ReposManager.CreateRepository("foo", "", "master"); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("case-variant create = %v, want ErrRefConflict", err)
	}
	mustCreate(t, "Foo2")
	mustAddAlias(t, "Foo2", "bar")
	if err := ReposManager.CreateRepository("BAR", "", "master"); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("case-variant alias create = %v, want ErrRefConflict", err)
	}
}

func TestAddAliasRejectsTakenRefs(t *testing.T) {
	newNamingManager(t)
	mustCreate(t, "a")
	mustCreate(t, "b")
	mustAddAlias(t, "b", "x/y")

	cases := []struct{ name, alias string }{
		{"a", "b"},   // 撞仓库名
		{"a", "x/y"}, // 撞别的仓库的别名
		{"a", "B"},   // 撞仓库名（大小写变体）
		{"a", "X/Y"}, // 撞别名（大小写变体）
		{"a", "A"},   // 撞自己的名字（大小写变体）
	}
	for _, c := range cases {
		if err := ReposManager.AddAlias(c.name, c.alias); !errors.Is(err, ErrRefConflict) {
			t.Errorf("AddAlias(%q, %q) = %v, want ErrRefConflict", c.name, c.alias, err)
		}
	}

	// 失败后状态不变
	repo, err := ReposManager.GetRepository("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.Aliases) != 1 || repo.Aliases[0] != "a" {
		t.Fatalf("aliases changed after failed AddAlias: %v", repo.Aliases)
	}
	if _, err := ReposManager.Resolve("b"); err != nil {
		t.Fatalf("b should stay resolvable: %v", err)
	}
}

func TestDeleteRepositoryReleasesRefs(t *testing.T) {
	newNamingManager(t)
	mustCreate(t, "r1")
	mustAddAlias(t, "r1", "shared")
	mustAddAlias(t, "r1", "team/shared")

	if err := ReposManager.DeleteRepository("r1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := ReposManager.Resolve("shared"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("Resolve(shared) after delete = %v, want ErrRepoNotFound", err)
	}
	mustCreate(t, "shared")
	if err := ReposManager.CreateRepository("team/shared", "", "master"); err == nil {
		t.Fatal("仓库名含斜杠应被拒绝")
	}
	if err := ReposManager.AddAlias("shared", "team/shared"); err != nil {
		t.Fatalf("别名可以多段: %v", err)
	}
}

func TestRemoveAliasReleasesRef(t *testing.T) {
	newNamingManager(t)
	mustCreate(t, "r1")
	mustAddAlias(t, "r1", "shared")

	if err := ReposManager.RemoveAlias("r1", "shared"); err != nil {
		t.Fatalf("remove alias: %v", err)
	}
	if _, err := ReposManager.Resolve("shared"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("Resolve(shared) after removal = %v, want ErrRepoNotFound", err)
	}
	mustCreate(t, "shared")
}

// ---------- 扫描期冲突：涉及仓库全部不可用 ----------

func TestScanDisablesConflictingRepositories(t *testing.T) {
	dir := newNamingManager(t)
	mustCreate(t, "a")
	mustCreate(t, "b")
	mustCreate(t, "c")

	// b 抢 a 的仓库名作为别名（正常路径已被拒绝，故直接改元数据）
	rewriteAliases(t, dir, "b", []string{"b", "a"})

	ReposManager = nil
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})

	for _, ref := range []string{"a", "b"} {
		if _, err := ReposManager.Resolve(ref); !errors.Is(err, ErrRepoNotFound) {
			t.Errorf("Resolve(%q) = %v, want ErrRepoNotFound (冲突仓库应全部禁用)", ref, err)
		}
	}
	if _, err := ReposManager.GetByAlias("a"); !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("GetByAlias(a) = %v, want ErrAliasNotFound", err)
	}
	if _, err := ReposManager.Resolve("c"); err != nil {
		t.Errorf("unrelated repo c must stay available: %v", err)
	}
	conflicts := ReposManager.NamingConflicts()
	if len(conflicts) != 1 || !strings.Contains(conflicts[0], "a") || !strings.Contains(conflicts[0], "b") {
		t.Fatalf("NamingConflicts() = %v, want one conflict mentioning a and b", conflicts)
	}
	if listed := len(ReposManager.List()); listed != 1 {
		t.Fatalf("List() = %d repos, want 1 (仅 c 可用)", listed)
	}
}

func TestScanRegistersNameRefInMemory(t *testing.T) {
	dir := newNamingManager(t)
	mustCreate(t, "x")
	rewriteAliases(t, dir, "x", []string{"only-alias"}) // 元数据丢了仓库名

	ReposManager = nil
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})

	if _, err := ReposManager.Resolve("x"); err != nil {
		t.Fatalf("Resolve(x) = %v, want ok（仓库名应为 ref）", err)
	}
	// 只在内存补齐，不改写磁盘
	data, err := os.ReadFile(filepath.Join(dir, "x.git", "pgit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct{ Aliases []string }
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Aliases) != 1 || meta.Aliases[0] != "only-alias" {
		t.Fatalf("metadata rewritten unexpectedly: %v", meta.Aliases)
	}
}

func TestScanDedupesRefsWithinRepository(t *testing.T) {
	dir := newNamingManager(t)
	mustCreate(t, "dup")
	rewriteAliases(t, dir, "dup", []string{"dup", "dup", "DUP"})

	ReposManager = nil
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})

	if repo, err := ReposManager.Resolve("dup"); err != nil || repo.Name != "dup" {
		t.Fatalf("Resolve(dup) = %v, %v", repo, err)
	}
	if conflicts := ReposManager.NamingConflicts(); len(conflicts) != 0 {
		t.Fatalf("同仓库内重复不应算冲突: %v", conflicts)
	}
}
