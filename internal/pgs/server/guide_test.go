package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// guidePathPattern 抓取文档里出现的 API 路径（查询串与标点之前的部分）。
var guidePathPattern = regexp.MustCompile(`/api/v1/[A-Za-z0-9._/{}-]*`)

// guidePlaceholder 去掉路径里的 {path...} 形态占位符。
var guidePlaceholder = regexp.MustCompile(`\{[^}]*\}`)

func TestGuideEndpointServesMarkdown(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	rec := do(h, http.MethodGet, GuidePath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("content-type = %q, want text/markdown prefix", ct)
	}
	body := rec.Body.String()
	if body != string(guideMarkdown) {
		t.Error("response body differs from embedded guide.md")
	}
	// 若这些关键词消失，说明文档骨架被改坏或 embed 未生效。
	for _, want := range []string{"# pgit", "git clone", "POST /api/v1/repos", "relay: stale base"} {
		if !strings.Contains(body, want) {
			t.Errorf("guide body missing %q", want)
		}
	}
}

// 文档里提到的每个 /api/v1/... 路径都必须真实存在（404 即文档漂移）。
func TestGuideDocumentedAPIPathsExist(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	checked := map[string]bool{}
	for _, raw := range guidePathPattern.FindAllString(string(guideMarkdown), -1) {
		p := guidePlaceholder.ReplaceAllString(strings.TrimRight(raw, ".,;:)"), "")
		if p == "" || checked[p] {
			continue
		}
		checked[p] = true
		// 用 GET 探测：POST-only 端点会回 405，同样证明路由存在；只把 404 当漂移。
		if rec := do(h, http.MethodGet, p); rec.Code == http.StatusNotFound {
			t.Errorf("guide mentions %q but that route does not exist (404)", p)
		}
	}
	if len(checked) < 15 {
		t.Fatalf("extracted only %d API paths from guide.md, expected >= 15 (path check likely broken)", len(checked))
	}
}

// 文档端点与其它管理 API 一样受 httpAuth 约束。
func TestGuideRespectsBasicAuth(t *testing.T) {
	h := newRouterHandler(t, "__webui", true)
	if rec := do(h, http.MethodGet, GuidePath); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when httpAuth is enabled", rec.Code)
	}
}
