package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"
)

// buildListRepo 在 dir 下建裸仓库并写入单个 commit（committer 时间戳由 ts 控制）
// 指向 master。ts<=0 表示不写对象（空仓库）。
func buildListRepo(t *testing.T, dir, name string, ts int64) *pgs.Repository {
	t.Helper()
	repo, err := pgs.InitBare(dir, name, "", "master")
	if err != nil {
		t.Fatalf("init %s: %v", name, err)
	}
	if ts <= 0 {
		return repo
	}
	oid, err := writeListCommit(t, repo, ts)
	if err != nil {
		t.Fatalf("commit %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(repo.Path(), "refs/heads/master"),
		[]byte(oid+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	return repo
}

// writeListCommit 写入 blob+tree+commit 三个 loose 对象，返回 commit oid hex。
func writeListCommit(t *testing.T, repo *pgs.Repository, ts int64) (string, error) {
	t.Helper()
	store := git.NewObjectStore(repo.Path())
	blob := git.NewRawObject(git.ObjBlob, []byte("content\n"))
	if _, err := store.Write(blob); err != nil {
		return "", err
	}
	tree := git.NewRawObject(git.ObjTree, treeBodyOneEntry(blob.Oid()))
	if _, err := store.Write(tree); err != nil {
		return "", err
	}
	body := fmt.Sprintf("tree %s\nauthor T <t@pgit.dev> %d +0000\ncommitter T <t@pgit.dev> %d +0000\n\ninit\n",
		tree.Oid(), ts, ts)
	commit := git.NewRawObject(git.ObjCommit, []byte(body))
	if _, err := store.Write(commit); err != nil {
		return "", err
	}
	return string(commit.Oid()), nil
}

// treeBodyOneEntry 编码只含一个 blob 的 tree 对象内容。
func treeBodyOneEntry(blobOid git.Oid) []byte {
	oid, _ := hex.DecodeString(string(blobOid))
	var buf bytes.Buffer
	buf.WriteString("100644 file.txt\x00")
	buf.Write(oid)
	return buf.Bytes()
}

// writeAnnotatedTag 在 repo 上建指向 commitOid 的 annotated tag（tagger 时间与
// commit 时间刻意错开，验证 peel 后取的是 commit 时间）。
func writeAnnotatedTag(t *testing.T, repo *pgs.Repository, commitOid string, taggerTs int64) string {
	t.Helper()
	store := git.NewObjectStore(repo.Path())
	body := fmt.Sprintf("object %s\ntype commit\ntag v1.0\ntagger T <t@pgit.dev> %d +0000\n\nrelease\n",
		commitOid, taggerTs)
	tag := git.NewRawObject(git.ObjTag, []byte(body))
	if _, err := store.Write(tag); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Path(), "refs/tags/v1.0"),
		[]byte(string(tag.Oid())+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	return string(tag.Oid())
}

// 列表按最后提交时间降序：B(2027) > delta(2025, 带 tag 但 tagger 是 2030) >
// A(2023) > 空仓库 gamma（无 lastCommitTime，最后）。创建顺序 A→B→gamma→delta，
// 与提交时间序相反，以区分「按创建时间排序」的旧逻辑。
func TestListReposSortedByLastCommitTime(t *testing.T) {
	dir := t.TempDir()
	buildListRepo(t, dir, "alpha", 1700000000)          // 2023-11-14
	buildListRepo(t, dir, "beta", 1800000000)           // 2027-01-15
	buildListRepo(t, dir, "gamma", 0)                   // 空仓库
	delta := buildListRepo(t, dir, "delta", 1750000000) // 2025-06-15
	master, err := os.ReadFile(filepath.Join(delta.Path(), "refs/heads/master"))
	if err != nil {
		t.Fatal(err)
	}
	writeAnnotatedTag(t, delta, strings.TrimSpace(string(master)), 1900000000) // tagger 2030-03-17，不得胜出

	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	h := NewHTTPHandler(pgs.ReposManager, &pgs.Setting{WebUIPrefix: "__webui"}, nil, nil)

	rec := do(h, http.MethodGet, "/api/v1/repos")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Total        int `json:"total"`
		Repositories []struct {
			Name           string     `json:"name"`
			LastCommitTime *time.Time `json:"lastCommitTime"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 4 || len(body.Repositories) != 4 {
		t.Fatalf("total = %d, len = %d", body.Total, len(body.Repositories))
	}
	wantOrder := []string{"beta", "delta", "alpha", "gamma"}
	for i, want := range wantOrder {
		if got := body.Repositories[i].Name; got != want {
			t.Fatalf("repositories[%d].name = %q, want %q (order: %v)", i, got, want, names(body.Repositories))
		}
	}
	wantTS := map[string]int64{
		"beta":  1800000000,
		"delta": 1750000000, // tag peel 后取 commit 时间，而非 tagger 的 1900000000
		"alpha": 1700000000,
	}
	for _, repo := range body.Repositories {
		want, ok := wantTS[repo.Name]
		if !ok {
			if repo.LastCommitTime != nil {
				t.Fatalf("%s: unexpected lastCommitTime %v", repo.Name, repo.LastCommitTime)
			}
			continue
		}
		if repo.LastCommitTime == nil {
			t.Fatalf("%s: lastCommitTime is nil", repo.Name)
		}
		if got := repo.LastCommitTime.UTC().Unix(); got != want {
			t.Fatalf("%s: lastCommitTime = %v, want %v", repo.Name, got, want)
		}
	}
}

func names(repos []struct {
	Name           string     `json:"name"`
	LastCommitTime *time.Time `json:"lastCommitTime"`
}) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Name)
	}
	return out
}
