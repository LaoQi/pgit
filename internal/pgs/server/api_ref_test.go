package server

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"
)

// seedRepo 建仓库并写入一个提交（file.txt + dir/nested.txt）与 master ref。
func seedRepo(t *testing.T, name, alias string) {
	t.Helper()
	if err := pgs.ReposManager.CreateRepository(name, "seeded", "master"); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	repo, err := pgs.ReposManager.GetRepository(name)
	if err != nil {
		t.Fatal(err)
	}
	store := &git.LooseStore{Root: filepath.Join(repo.Path(), "objects")}
	blob := git.NewRawObject(git.ObjBlob, []byte("hello\n"))
	nested := git.NewRawObject(git.ObjBlob, []byte("nested\n"))
	sub := git.NewRawObject(git.ObjTree, treeBody(func(add func(mode int, name string, oid git.Oid)) {
		add(0o100644, "nested.txt", nested.Oid())
	}))
	top := git.NewRawObject(git.ObjTree, treeBody(func(add func(mode int, name string, oid git.Oid)) {
		add(0o100644, "file.txt", blob.Oid())
		add(0o040000, "dir", sub.Oid())
	}))
	commit := git.NewRawObject(git.ObjCommit, []byte(fmt.Sprintf(
		"tree %s\nauthor Test <t@pgit.dev> 1700000000 +0800\ncommitter Test <t@pgit.dev> 1700000000 +0800\n\ninitial\n", top.Oid())))
	for _, o := range []*git.RawObject{blob, nested, sub, top, commit} {
		if _, err := store.Write(o); err != nil {
			t.Fatalf("write object: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo.Path(), "refs/heads/master"), []byte(commit.Oid()+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if alias != "" {
		if err := pgs.ReposManager.AddAlias(name, alias); err != nil {
			t.Fatalf("add alias %s: %v", alias, err)
		}
	}
}

// treeBody 编码 tree 对象内容。
func treeBody(fill func(add func(mode int, name string, oid git.Oid))) []byte {
	var buf []byte
	fill(func(mode int, name string, oid git.Oid) {
		buf = append(buf, []byte(fmt.Sprintf("%o %s", mode, name))...)
		buf = append(buf, 0)
		b, _ := hex.DecodeString(string(oid))
		buf = append(buf, b...)
	})
	return buf
}

// 按名字与按别名访问内容接口，结果必须等价（多段别名不编码斜杠）。
func TestAPIRefEquivalenceByNameAndAlias(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	seedRepo(t, "demo", "team/demo")

	cases := []struct{ path, wantCT string }{
		{"/api/v1/repos/info", "application/json"},
		{"/api/v1/repos/tree", "application/json"},
		{"/api/v1/repos/blob/file.txt", "text/plain"},
		{"/api/v1/repos/commits", "application/json"},
		{"/api/v1/repos/archive", "application/octet-stream"},
	}
	for _, c := range cases {
		byName := do(h, http.MethodGet, c.path+"?ref=demo&treeish=master")
		byAlias := do(h, http.MethodGet, c.path+"?ref=team/demo&treeish=master")
		if byName.Code != http.StatusOK {
			t.Errorf("%s by name = %d, want 200 (%s)", c.path, byName.Code, byName.Body.String())
			continue
		}
		if byAlias.Code != http.StatusOK {
			t.Errorf("%s by alias = %d, want 200 (%s)", c.path, byAlias.Code, byAlias.Body.String())
			continue
		}
		if byName.Body.String() != byAlias.Body.String() {
			t.Errorf("%s by name != by alias", c.path)
		}
		if ct := byAlias.Header().Get("Content-Type"); !strings.Contains(ct, c.wantCT) {
			t.Errorf("%s by alias Content-Type = %q, want %s", c.path, ct, c.wantCT)
		}
	}

	// info 返回 canonical name（别名不改变响应体里的 name）
	rec := do(h, http.MethodGet, "/api/v1/repos/info?ref=team/demo")
	if !strings.Contains(rec.Body.String(), `"name":"demo"`) {
		t.Errorf("info by alias body = %s, want canonical name", rec.Body.String())
	}
}

// 写操作按别名寻址：settings / default-branch / aliases 增删。
func TestAPIWriteByAlias(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	seedRepo(t, "demo", "team/demo")

	if rec := do(h, http.MethodPost, "/api/v1/repos/settings?ref=team/demo&description=via+alias"); rec.Code != http.StatusOK {
		t.Fatalf("settings by alias = %d (%s)", rec.Code, rec.Body.String())
	}
	if repo, err := pgs.ReposManager.GetRepository("demo"); err != nil || repo.Description != "via alias" {
		t.Fatalf("description not updated: %+v err=%v", repo, err)
	}
	if rec := do(h, http.MethodPost, "/api/v1/repos/default-branch?ref=team/demo&branch=master"); rec.Code != http.StatusOK {
		t.Fatalf("default-branch by alias = %d (%s)", rec.Code, rec.Body.String())
	}

	// 多段别名：加（字面斜杠，不编码）与删
	if rec := do(h, http.MethodPost, "/api/v1/repos/aliases?ref=team/demo&alias=grp/sub/deep"); rec.Code != http.StatusOK {
		t.Fatalf("add multi-segment alias = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := pgs.ReposManager.Resolve("grp/sub/deep"); err != nil {
		t.Fatalf("multi-segment alias not resolvable: %v", err)
	}
	if rec := do(h, http.MethodDelete, "/api/v1/repos/aliases?ref=demo&alias=grp/sub/deep"); rec.Code != http.StatusOK {
		t.Fatalf("delete multi-segment alias = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := pgs.ReposManager.Resolve("grp/sub/deep"); err == nil {
		t.Fatal("multi-segment alias still resolvable after delete")
	}

	// 与已有 ref 冲突 → 409
	if rec := do(h, http.MethodPost, "/api/v1/repos/aliases?ref=demo&alias=demo"); rec.Code != http.StatusConflict {
		t.Errorf("conflicting alias = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

// 删除：confirm 必须等于 canonical name，即使请求用别名寻址。
func TestAPIDeleteConfirmUsesCanonicalName(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	seedRepo(t, "demo", "team/demo")

	rec := do(h, http.MethodDelete, "/api/v1/repos/info?ref=team/demo&confirm=team/demo")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expected demo") {
		t.Fatalf("confirm with alias = %d %s, want 400 mentioning demo", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/api/v1/repos/info?ref=team/demo&confirm=demo"); rec.Code != http.StatusOK {
		t.Fatalf("confirm with canonical name = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := pgs.ReposManager.Resolve("team/demo"); err == nil {
		t.Fatal("repo still resolvable after delete")
	}
}

// 未知 ref → 404；缺 ref → 400。
func TestAPIRefErrors(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	seedRepo(t, "demo", "")

	if rec := do(h, http.MethodGet, "/api/v1/repos/info?ref=nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown ref = %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/repos/info"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing ref = %d, want 400", rec.Code)
	}
}
