package pgs

import (
	"strings"
	"testing"
)

func TestRegistryRenderPrometheusFormat(t *testing.T) {
	r := NewRegistry()
	r.Counter("pgit_test_total", "测试计数。", "kind").Add(3, "a")
	r.Counter("pgit_test_total", "测试计数。", "kind").Inc("b")
	r.Gauge("pgit_test_gauge", "测试瞬时值。").Set(1.5)

	out := string(r.Render())
	for _, want := range []string{
		"# HELP pgit_test_total 测试计数。",
		"# TYPE pgit_test_total counter",
		`pgit_test_total{kind="a"} 3`,
		`pgit_test_total{kind="b"} 1`,
		"# TYPE pgit_test_gauge gauge",
		"pgit_test_gauge 1.5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered metrics missing %q\n---\n%s", want, out)
		}
	}
}

// 标签值需转义（引号/反斜杠/换行），否则 Prometheus 解析失败。
func TestRegistryLabelEscaping(t *testing.T) {
	r := NewRegistry()
	r.Counter("pgit_esc_total", "转义测试。", "path").Inc(`a"b\c` + "\n")

	out := string(r.Render())
	if !strings.Contains(out, `pgit_esc_total{path="a\"b\\c\n"} 1`) {
		t.Errorf("label not escaped properly:\n%s", out)
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("pgit_conc_total", "并发测试。")
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 500; j++ {
				c.Inc()
				_ = r.Render()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := string(r.Render()); !strings.Contains(got, "pgit_conc_total 4000") {
		t.Errorf("counter value lost under concurrency:\n%s", got)
	}
}

// 默认表上的辅助函数不应 panic，且能被渲染。
func TestDefaultRegistryHelpers(t *testing.T) {
	ObserveHTTPRequest("GET", 200, 1000)
	ObserveHTTPRequest("POST", 500, 2000)
	ObserveGitOperation("upload-pack", "success")
	ObservePackBytes("upload-pack", 1024)
	ObservePackBytes("receive-pack", 0) // <=0 应忽略
	SetMirrorSyncResult("repo1", true, 10, 42)
	SetMirrorSyncResult("repo1", false, 0, 7)
	SetReposTotal(3)
	PackStarted()
	PackFinished()

	out := string(DefaultRegistry().Render())
	for _, want := range []string{
		"pgit_http_requests_total",
		`pgit_http_requests_total{method="GET",status="200",result="ok"} 1`,
		`pgit_http_requests_total{method="POST",status="500",result="error"} 1`,
		"pgit_git_operations_total",
		`pgit_pack_bytes_total{service="upload-pack"} 1024`,
		`pgit_mirror_sync_total{repo="repo1",result="success"} 1`,
		`pgit_mirror_sync_total{repo="repo1",result="failure"} 1`,
		"pgit_repositories_total 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in default registry output", want)
		}
	}
	if strings.Contains(out, `service="receive-pack"`) {
		t.Error("zero-byte pack should not be recorded")
	}
	if n := InflightPacks(); n != 0 {
		t.Errorf("InflightPacks = %d, want 0", n)
	}
}
