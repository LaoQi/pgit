package server

import (
	_ "embed"
	"net/http"
)

// GuidePath 是内建 markdown 文档（本服务的接口与用法说明）的访问路径。
// 文档随二进制发布，仅供读取；内容真源是 internal/pgs/server/guide.md。
const GuidePath = "/api/v1/guide.md"

//go:embed guide.md
var guideMarkdown []byte

// serveGuide 返回内建 markdown 原文（不做渲染），供 agent 直接读取。
func (h *HTTPHandler) serveGuide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(guideMarkdown)
}
