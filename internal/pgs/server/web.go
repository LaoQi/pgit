package server

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed web
var webFS embed.FS

var indexTemplate []byte

func init() {
	indexTemplate, _ = webFS.ReadFile("web/index.html")
}

// ExportWebUI writes embedded webui files to dir.
func ExportWebUI(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := webFS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "web/")
		target := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

func (h *HTTPHandler) webFileSystem() http.FileSystem {
	if h.Settings.WebUIAssets != "" {
		return http.Dir(h.Settings.WebUIAssets)
	}
	sub, _ := fs.Sub(webFS, "web")
	return http.FS(sub)
}

func (h *HTTPHandler) serveWebUI(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + h.Settings.WebUIPrefix
	p := strings.TrimPrefix(r.URL.Path, prefix)
	p = strings.TrimPrefix(p, "/")

	if p != "" {
		fsys := h.webFileSystem()
		f, err := fsys.Open(p)
		if err == nil {
			defer f.Close()
			stat, err := f.Stat()
			if err == nil && !stat.IsDir() {
				http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
				return
			}
		}
	}

	h.serveIndexHTML(w, r)
}

func (h *HTTPHandler) serveIndexHTML(w http.ResponseWriter, r *http.Request) {
	var htmlData []byte
	if h.Settings.WebUIAssets != "" {
		data, err := os.ReadFile(filepath.Join(h.Settings.WebUIAssets, "index.html"))
		if err == nil {
			htmlData = data
		}
	}
	if htmlData == nil {
		htmlData = indexTemplate
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	htmlData = bytes.Replace(htmlData, []byte("__WEBUI_PREFIX__"), []byte(h.Settings.WebUIPrefix), -1)
	_, _ = w.Write(htmlData)
}
