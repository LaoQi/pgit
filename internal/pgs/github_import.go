package pgs

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// maxGithubImportRepos 单次导入的仓库数上限（防止一次勾选把实例撑爆）。
const maxGithubImportRepos = 200

// GithubImportRequest 是一次「从 GitHub 账号导入镜像仓库」的请求。
// 账号信息只在导入时使用一次：Token 会写进每个仓库的 MirrorConfig，
// 之后这些仓库与手工创建的镜像仓库完全一致（无账号实体、无额外调度）。
type GithubImportRequest struct {
	Owner        string
	Token        string // 空 = 仅公开仓库（私有仓库会导入失败）
	APIBase      string // 空 = https://api.github.com
	CloneBase    string // 空 = 用 GitHub 返回的 clone_url（GHE 自动正确）
	Proxy        string
	NamePrefix   string
	SyncInterval int
	Repos        []string // 仓库名（也接受 owner/repo 形式）
}

// GithubImportResult 是单个仓库的导入结果。
type GithubImportResult struct {
	Repo        string   `json:"repo"`
	LocalName   string   `json:"localName,omitempty"`
	RemoteURL   string   `json:"remoteUrl,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Description string   `json:"description,omitempty"`
	OK          bool     `json:"ok"`
	Error       string   `json:"error,omitempty"`
	Warning     string   `json:"warning,omitempty"`
}

// AnnotateGithubRepos 为发现结果补上本地命名建议与冲突标注（只读，不改动任何仓库）。
func (r *RepositoriesManager) AnnotateGithubRepos(repos []GithubRepo, namePrefix string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range repos {
		if repos[i].LocalName == "" {
			repos[i].LocalName = GithubLocalName(namePrefix, repos[i].Owner, repos[i].Name)
		}
		repos[i].Conflict = ""
		if repo, ok := r.byName[repos[i].LocalName]; ok {
			repos[i].Conflict = conflictCode(repo, repos[i].CloneURL)
			continue
		}
		if _, ok := r.byAlias[repos[i].Owner+"/"+repos[i].Name]; ok {
			repos[i].Conflict = "alias-exists"
		}
	}
}

// conflictCode 返回本地同名仓库造成的冲突类型（"" 表示无冲突）。
func conflictCode(repo *Repository, remoteURL string) string {
	switch {
	case !repo.IsMirror():
		return "not-a-mirror"
	case sameRemoteURL(repo.Mirror.RemoteURL, remoteURL):
		return "mirror-same-remote"
	default:
		return "mirror-other-remote"
	}
}

// sameRemoteURL 比较两个远端 URL 是否指向同一仓库（忽略大小写、结尾 / 与 .git）。
func sameRemoteURL(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimRight(strings.TrimSpace(s), "/")
		s = strings.TrimSuffix(s, ".git")
		return strings.ToLower(s)
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// ImportGithubMirrors 为选中的 GitHub 仓库创建镜像仓库，并注册定时同步。
// 逐仓库独立处理：单个失败不影响其他仓库，结果数组与请求顺序一致。
func ImportGithubMirrors(manager *RepositoriesManager, syncMgr *SyncManager, req GithubImportRequest) ([]GithubImportResult, error) {
	if manager == nil {
		return nil, fmt.Errorf("repository manager is not initialized")
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" {
		return nil, fmt.Errorf("github owner is required")
	}
	if err := validateGithubOwner(owner); err != nil {
		return nil, err
	}
	if req.SyncInterval < 0 {
		return nil, fmt.Errorf("syncInterval must be >= 0")
	}

	names, err := normalizeGithubSelection(req.Repos, owner)
	if err != nil {
		return nil, err
	}

	cloneBase := strings.TrimRight(strings.TrimSpace(req.CloneBase), "/")
	if cloneBase != "" {
		if err := validateCloneBase(cloneBase); err != nil {
			return nil, err
		}
	}

	// 发现账号全部仓库（含 fork/archived，避免勾选的仓库在二次过滤中被丢掉）
	all, err := DiscoverGithubRepos(GithubDiscoverQuery{
		Owner:           owner,
		Token:           req.Token,
		APIBase:         req.APIBase,
		Proxy:           req.Proxy,
		IncludeForks:    true,
		IncludeArchived: true,
		NamePrefix:      req.NamePrefix,
	})
	if err != nil {
		return nil, err
	}
	byName := make(map[string]GithubRepo, len(all))
	byFull := make(map[string]GithubRepo, len(all))
	for _, r := range all {
		byName[strings.ToLower(r.Name)] = r
		byFull[strings.ToLower(r.FullName)] = r
	}

	results := make([]GithubImportResult, 0, len(names))
	for _, want := range names {
		res := GithubImportResult{Repo: want}
		remote, ok := byName[strings.ToLower(want)]
		if !ok {
			remote, ok = byFull[strings.ToLower(want)]
		}
		if !ok {
			// 发现列表可能因分页中断/瞬时不一致而缺项：回退到单仓库接口确认。
			if fetched, ferr := FetchGithubRepo(GithubDiscoverQuery{
				Owner: owner, Token: req.Token, APIBase: req.APIBase, Proxy: req.Proxy, NamePrefix: req.NamePrefix,
			}, want); ferr == nil {
				remote, ok = *fetched, true
			} else {
				res.Error = "not found in account (check the repository name and token access)"
				results = append(results, res)
				continue
			}
		}

		remoteURL := strings.TrimSpace(remote.CloneURL)
		if cloneBase != "" {
			remoteURL = cloneBase + "/" + remote.FullName + ".git"
		}
		if remoteURL == "" {
			res.Error = "remote clone URL is empty"
			results = append(results, res)
			continue
		}

		localName := GithubLocalName(req.NamePrefix, remote.Owner, remote.Name)
		alias := remote.Owner + "/" + remote.Name
		res.LocalName, res.RemoteURL, res.Description = localName, remoteURL, remote.Description

		if err := ValidateRepoName(localName); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		if existing, err := manager.GetRepository(localName); err == nil {
			res.Error = conflictMessage(existing, remoteURL)
			results = append(results, res)
			continue
		}
		if err := ValidateAlias(alias); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		if alias != localName {
			if existing, err := manager.GetByAlias(alias); err == nil {
				res.Error = fmt.Sprintf("alias %s is already used by repository %s", alias, existing.Name)
				results = append(results, res)
				continue
			}
		}

		// Token 只落盘到 private 仓库的镜像配置：public 仓库匿名可 fetch，
		// 避免同一 token 被复制到几十个 pgit.json（仓库转私后可用 settings 接口补）。
		authType, username, password := "none", "", ""
		if req.Token != "" && remote.Private {
			authType, username, password = "basic", "x-access-token", req.Token
		}
		mirror := &MirrorConfig{
			RemoteURL:    remoteURL,
			SyncInterval: req.SyncInterval,
			AuthType:     authType,
			Username:     username,
			Password:     password,
			Proxy:        req.Proxy,
		}
		if err := manager.CreateMirrorRepositoryWithBranch(localName, remote.Description, mirror, remote.DefaultBranch); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}

		aliases := []string{localName}
		if alias != localName {
			if err := manager.AddAlias(localName, alias); err != nil {
				res.Warning = fmt.Sprintf("alias %s not added: %v", alias, err)
			} else {
				aliases = append(aliases, alias)
			}
		}
		res.OK, res.Aliases = true, aliases

		if syncMgr != nil {
			if snap, err := manager.GetRepository(localName); err == nil {
				// SyncInterval>0：Register 自带首次同步；=0（仅手动）：额外触发一次，
				// 否则导入出来的是空壳仓库。
				syncMgr.Register(snap)
				if req.SyncInterval <= 0 {
					if err := syncMgr.SyncAsync(localName); err != nil && !errors.Is(err, ErrSyncInProgress) {
						slog.Warn("github mirror initial sync not queued", "repo", localName, "error", err)
					}
				}
			}
		}
		slog.Info("github mirror created", "repo", localName, "remote", remoteURL, "syncInterval", req.SyncInterval)
		results = append(results, res)
	}
	return results, nil
}

// normalizeGithubSelection 清洗勾选列表：去空、去重（保序），支持 owner/repo 写法，并做上限校验。
func normalizeGithubSelection(repos []string, owner string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(repos))
	for _, raw := range repos {
		name := strings.TrimSpace(raw)
		name = strings.TrimSuffix(strings.TrimPrefix(name, owner+"/"), ".git")
		if name == "" {
			continue
		}
		if strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid repository name %q (expected a repository name, not a path)", raw)
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no repositories selected")
	}
	if len(out) > maxGithubImportRepos {
		return nil, fmt.Errorf("too many repositories selected: %d (max %d)", len(out), maxGithubImportRepos)
	}
	return out, nil
}

func validateCloneBase(cloneBase string) error {
	u, err := url.Parse(cloneBase)
	if err != nil {
		return fmt.Errorf("invalid cloneBase: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("cloneBase must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("cloneBase must include a host")
	}
	return nil
}

// conflictMessage 说明本地同名仓库为何不能导入。
func conflictMessage(existing *Repository, remoteURL string) string {
	switch conflictCode(existing, remoteURL) {
	case "not-a-mirror":
		return fmt.Sprintf("a local repository named %s already exists (not a mirror)", existing.Name)
	case "mirror-same-remote":
		return fmt.Sprintf("already mirrored from %s", remoteURL)
	default:
		return fmt.Sprintf("a local mirror named %s already exists (remote %s)", existing.Name, existing.Mirror.RemoteURL)
	}
}

// SummarizeGithubImport 统计导入结果。
func SummarizeGithubImport(results []GithubImportResult) (created, failed int) {
	for _, r := range results {
		if r.OK {
			created++
		} else {
			failed++
		}
	}
	return
}
