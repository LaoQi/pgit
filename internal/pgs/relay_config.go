package pgs

import (
	"fmt"
	"strings"
)

// 远端配置模式。空字符串等价 MirrorModePull（老元数据兼容）。
const (
	// MirrorModePull 镜像仓库：只从上游拉取，禁止 push。
	MirrorModePull = "pull"
	// MirrorModeRelay 中转仓库：上游为基线，下游快进推送准入后转发上游。
	MirrorModeRelay = "relay"
)

// DefaultRelayRefPrefixes 是中转仓库默认的 ref 白名单前缀：分支与 tag 均允许
// （新分支、新 tag 是正常操作）；其他命名空间（refs/notes 等）拒绝。
var DefaultRelayRefPrefixes = []string{"refs/heads/", "refs/tags/"}

// IsRelay 判断仓库是否为中转仓库。
func (repo *Repository) IsRelay() bool {
	return repo != nil && repo.Mirror != nil && repo.Mirror.Mode == MirrorModeRelay
}

// AllowsDelete 报告是否允许把下游的 ref 删除转发到上游（默认允许）。
func (m *MirrorConfig) AllowsDelete() bool {
	return m == nil || m.AllowDelete == nil || *m.AllowDelete
}

// ForwardRefPrefixes 返回准入/转发白名单前缀；未配置时为默认值。
func (m *MirrorConfig) ForwardRefPrefixes() []string {
	if m == nil || len(m.Refs) == 0 {
		return DefaultRelayRefPrefixes
	}
	return m.Refs
}

// AcceptsRef 判断 ref 是否落在白名单内（前缀匹配）。
func (m *MirrorConfig) AcceptsRef(name string) bool {
	for _, p := range m.ForwardRefPrefixes() {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// validateRelayRefs 校验中转仓库的 ref 白名单前缀。
func validateRelayRefs(refs []string) error {
	for _, p := range refs {
		if p == "" {
			return fmt.Errorf("relay ref prefix must not be empty")
		}
		if !strings.HasPrefix(p, "refs/") {
			return fmt.Errorf("relay ref prefix %q must start with \"refs/\"", p)
		}
		if strings.ContainsAny(p, " \t\r\n") {
			return fmt.Errorf("relay ref prefix %q must not contain whitespace", p)
		}
		if strings.Contains(p, "..") {
			return fmt.Errorf("relay ref prefix %q must not contain \"..\"", p)
		}
	}
	return nil
}
