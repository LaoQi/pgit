package git

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func makeEmptyLocalRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newFetchTestServer(t *testing.T, gitRoot string) *httptest.Server {
	t.Helper()
	return newFetchTestServerWithAuth(t, gitRoot, "", "")
}

func newFetchTestServerWithAuth(t *testing.T, gitRoot, user, pass string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authCheck := func(r *http.Request) bool {
		if user == "" {
			return true
		}
		u, p, ok := r.BasicAuth()
		return ok && u == user && p == pass
	}
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		if !authCheck(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="pgit"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		service := r.URL.Query().Get("service")
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		out, err := ServeInfoRefs(gitRoot, service)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(out)
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		if !authCheck(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		if err := HandleUploadPack(gitRoot, r.Body, w); err != nil {
			t.Logf("upload-pack: %v", err)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func assertObjectExists(t *testing.T, repoRoot string, oid Oid) {
	t.Helper()
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	if !store.Exists(oid) {
		t.Errorf("object %s not found in local store", oid)
	}
}

func assertRefEquals(t *testing.T, repoRoot, refName string, expectedOid Oid) {
	t.Helper()
	rs := NewRefStore(repoRoot)
	oid, err := rs.Get(refName)
	if err != nil {
		t.Errorf("Get %s: %v", refName, err)
		return
	}
	if oid != expectedOid {
		t.Errorf("%s = %s, want %s", refName, oid, expectedOid)
	}
}

func assertRefDeleted(t *testing.T, repoRoot, refName string) {
	t.Helper()
	rs := NewRefStore(repoRoot)
	_, err := rs.Get(refName)
	if err == nil {
		t.Errorf("ref %s should be deleted", refName)
	}
}

func TestFetchRemote_InitialClone(t *testing.T) {
	remoteDir, commitOid := makeRepoWithCommit(t)
	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)

	result, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil)
	if err != nil {
		t.Fatalf("FetchRemote: %v", err)
	}
	if result.UpToDate {
		t.Errorf("UpToDate = true, want false for initial clone")
	}
	if result.ObjectsWritten != 3 {
		t.Errorf("ObjectsWritten = %d, want 3", result.ObjectsWritten)
	}
	if result.RefsUpdated != 1 {
		t.Errorf("RefsUpdated = %d, want 1", result.RefsUpdated)
	}

	blob := makeBlob("hello pgit\n")
	tree := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob.Oid()},
	})
	assertObjectExists(t, localRoot, commitOid)
	assertObjectExists(t, localRoot, tree.Oid())
	assertObjectExists(t, localRoot, blob.Oid())
	assertRefEquals(t, localRoot, "refs/heads/master", commitOid)

	rs := NewRefStore(localRoot)
	head, err := rs.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != "refs/heads/master" {
		t.Errorf("HEAD = %q, want refs/heads/master", head)
	}
}

func TestFetchRemote_IncrementalSync(t *testing.T) {
	remoteDir, commit1Oid := makeRepoWithCommit(t)
	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)

	result1, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil)
	if err != nil {
		t.Fatalf("initial FetchRemote: %v", err)
	}
	if result1.ObjectsWritten != 3 {
		t.Fatalf("initial ObjectsWritten = %d, want 3", result1.ObjectsWritten)
	}

	blob1 := makeBlob("hello pgit\n")
	blob2 := makeBlob("new file\n")
	tree2 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit2 := makeCommit(tree2.Oid(), []Oid{commit1Oid}, "second commit\n")
	store := &LooseStore{Root: filepath.Join(remoteDir, "objects")}
	writeAll(t, store, blob2, tree2, commit2)
	rs := NewRefStore(remoteDir)
	if _, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: commit1Oid, NewOid: commit2.Oid()},
	}); err != nil {
		t.Fatalf("update remote ref: %v", err)
	}

	result2, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil)
	if err != nil {
		t.Fatalf("incremental FetchRemote: %v", err)
	}
	if result2.UpToDate {
		t.Errorf("UpToDate = true, want false for incremental sync")
	}
	if result2.ObjectsWritten != 3 {
		t.Errorf("ObjectsWritten = %d, want 3 (commit2+tree2+blob2)", result2.ObjectsWritten)
	}
	if result2.RefsUpdated != 1 {
		t.Errorf("RefsUpdated = %d, want 1", result2.RefsUpdated)
	}

	assertObjectExists(t, localRoot, commit2.Oid())
	assertObjectExists(t, localRoot, tree2.Oid())
	assertObjectExists(t, localRoot, blob2.Oid())
	assertObjectExists(t, localRoot, commit1Oid)
	assertRefEquals(t, localRoot, "refs/heads/master", commit2.Oid())
}

func TestFetchRemote_UpToDate(t *testing.T) {
	remoteDir, _ := makeRepoWithCommit(t)
	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)

	if _, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil); err != nil {
		t.Fatalf("initial FetchRemote: %v", err)
	}

	result, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil)
	if err != nil {
		t.Fatalf("second FetchRemote: %v", err)
	}
	if !result.UpToDate {
		t.Errorf("UpToDate = false, want true")
	}
	if result.ObjectsWritten != 0 {
		t.Errorf("ObjectsWritten = %d, want 0", result.ObjectsWritten)
	}
}

func TestFetchRemote_EmptyRemote(t *testing.T) {
	remoteDir := makeEmptyRepoWithHead(t)
	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)

	result, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil)
	if err != nil {
		t.Fatalf("FetchRemote: %v", err)
	}
	if !result.UpToDate {
		t.Errorf("UpToDate = false, want true for empty remote")
	}
}

func TestFetchRemote_BasicAuth(t *testing.T) {
	remoteDir, commitOid := makeRepoWithCommit(t)
	ts := newFetchTestServerWithAuth(t, remoteDir, "user", "pass")
	localRoot := makeEmptyLocalRepo(t)

	auth := &FetchAuth{Type: "basic", Username: "user", Password: "pass"}
	result, err := FetchRemote(ts.URL+"/repo.git", localRoot, auth)
	if err != nil {
		t.Fatalf("FetchRemote with auth: %v", err)
	}
	if result.ObjectsWritten != 3 {
		t.Errorf("ObjectsWritten = %d, want 3", result.ObjectsWritten)
	}
	assertRefEquals(t, localRoot, "refs/heads/master", commitOid)
}

func TestFetchRemote_BasicAuthFailure(t *testing.T) {
	remoteDir, _ := makeRepoWithCommit(t)
	ts := newFetchTestServerWithAuth(t, remoteDir, "user", "pass")
	localRoot := makeEmptyLocalRepo(t)

	auth := &FetchAuth{Type: "basic", Username: "wrong", Password: "wrong"}
	_, err := FetchRemote(ts.URL+"/repo.git", localRoot, auth)
	if err == nil {
		t.Fatal("FetchRemote should fail with wrong credentials")
	}
}

func TestFetchRemote_RefDeletion(t *testing.T) {
	remoteDir, commit1Oid := makeRepoWithCommit(t)
	rs := NewRefStore(remoteDir)
	if _, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/feature", OldOid: ZeroOid, NewOid: commit1Oid},
	}); err != nil {
		t.Fatalf("create feature branch: %v", err)
	}

	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)

	if _, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil); err != nil {
		t.Fatalf("initial FetchRemote: %v", err)
	}
	assertRefEquals(t, localRoot, "refs/heads/feature", commit1Oid)

	os.Remove(filepath.Join(remoteDir, "refs/heads", "feature"))

	blob1 := makeBlob("hello pgit\n")
	blob2 := makeBlob("new file\n")
	tree2 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit2 := makeCommit(tree2.Oid(), []Oid{commit1Oid}, "second commit\n")
	store := &LooseStore{Root: filepath.Join(remoteDir, "objects")}
	writeAll(t, store, blob2, tree2, commit2)
	if _, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: commit1Oid, NewOid: commit2.Oid()},
	}); err != nil {
		t.Fatalf("update remote master: %v", err)
	}

	result, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil)
	if err != nil {
		t.Fatalf("FetchRemote after deletion: %v", err)
	}
	if result.RefsDeleted != 1 {
		t.Errorf("RefsDeleted = %d, want 1", result.RefsDeleted)
	}
	assertRefDeleted(t, localRoot, "refs/heads/feature")
	assertRefEquals(t, localRoot, "refs/heads/master", commit2.Oid())
}
