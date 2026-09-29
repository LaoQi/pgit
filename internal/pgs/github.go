package pgs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// GitHub 发现（只读）与导入（生成镜像仓库）相关常量。
const (
	defaultGithubAPIBase = "https://api.github.com"
	githubPerPage        = 100
	githubMaxPages       = 100 // 单账号最多翻 100 页（1 万仓库）
	githubRequestTimeout = 30 * time.Second
	githubRetryAttempts  = 3
)

// GithubRepo 是一个 GitHub 仓库的发现结果（含本地命名建议与冲突标注）。
type GithubRepo struct {
	FullName      string `json:"fullName"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	Description   string `json:"description,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	SizeKB        int64  `json:"sizeKb"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
	CloneURL      string `json:"cloneUrl"`

	// LocalName 是建议的本地仓库名（{namePrefix}{owner}_{repo}）。
	LocalName string `json:"localName"`
	// Conflict 标注本地是否已有同名仓库/同名 alias，为空表示可导入。
	// 取值：not-a-mirror | mirror-other-remote | mirror-same-remote | alias-exists
	Conflict string `json:"conflict,omitempty"`
}

// GithubDiscoverQuery 是账号仓库发现请求。Token 为空时只返回公开仓库。
type GithubDiscoverQuery struct {
	Owner           string
	Token           string
	APIBase         string // 空 = https://api.github.com（可指向 GHE / 测试用假 API）
	Proxy           string
	IncludeForks    bool
	IncludeArchived bool
	NamePrefix      string
}

// GithubLocalName 返回本地仓库名：{namePrefix}{owner}_{repo}。
// GitHub 的 owner 不含下划线，因此该命名可逆且同一账号内唯一。
func GithubLocalName(namePrefix, owner, repo string) string {
	return namePrefix + owner + "_" + repo
}

// DiscoverGithubRepos 列出账号（用户或组织）下的仓库。
//
// 无 Token 时只有公开仓库；带 Token 且是账号本人时走 /user/repos，
// 从而包含私有仓库。分页按 Link 头的 rel="next" 驱动。
func DiscoverGithubRepos(q GithubDiscoverQuery) ([]GithubRepo, error) {
	owner := strings.TrimSpace(q.Owner)
	if owner == "" {
		return nil, fmt.Errorf("github owner is required")
	}
	if err := validateGithubOwner(owner); err != nil {
		return nil, err
	}
	api := strings.TrimRight(strings.TrimSpace(q.APIBase), "/")
	if api == "" {
		api = defaultGithubAPIBase
	}

	client, err := newGithubClient(q.Proxy)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()

	// 本人账号必须走 /user/repos，/users/{owner}/repos 拿不到私有仓库。
	var endpoints []string
	if q.Token != "" {
		login, err := githubLogin(client, api, q.Token)
		if err != nil {
			var he *GithubAPIError
			// 明确是凭据问题就报错，避免用户以为「私有仓库拉不到」是权限之外的原因。
			if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
				return nil, err // 保留 *GithubAPIError，上层据此回 401
			}
			login = ""
		}
		if strings.EqualFold(login, owner) {
			// affiliation=owner：只要本人拥有的仓库。默认值会带上「协作者/组织成员」的仓库，
			// 那不是「这个账号的仓库」。
			endpoints = append(endpoints, api+"/user/repos?affiliation=owner&visibility=all")
		}
	}
	if len(endpoints) == 0 {
		// 用户 → 组织：GitHub 对组织名不会返回用户的接口，反之亦然，按顺序回退。
		// 用户入口不传 type（默认 owner = 该用户拥有的仓库，含本人可见的私有）；
		// 组织入口 type=all（组织拥有的全部仓库，成员可见私有）。
		endpoints = append(endpoints, fmt.Sprintf("%s/users/%s/repos", api, url.PathEscape(owner)))
		endpoints = append(endpoints, fmt.Sprintf("%s/orgs/%s/repos?type=all", api, url.PathEscape(owner)))
	}

	var apiRepos []githubAPIRepo
	var lastErr error
	for i, ep := range endpoints {
		list, err := githubListRepos(client, ep, q.Token)
		if err == nil {
			apiRepos = list
			lastErr = nil
			break
		}
		lastErr = err
		var he *GithubAPIError
		if errors.As(err, &he) && he.Status == http.StatusNotFound && i < len(endpoints)-1 {
			continue // 试下一个入口（用户 → 组织）
		}
		return nil, err
	}
	if lastErr != nil {
		return nil, lastErr
	}

	out := make([]GithubRepo, 0, len(apiRepos))
	for _, r := range apiRepos {
		if r.Disabled {
			continue
		}
		if !q.IncludeForks && r.Fork {
			continue
		}
		if !q.IncludeArchived && r.Archived {
			continue
		}
		ownerLogin := r.Owner.Login
		if ownerLogin == "" {
			ownerLogin = owner
		}
		out = append(out, GithubRepo{
			FullName:      r.FullName,
			Name:          r.Name,
			Owner:         ownerLogin,
			Private:       r.Private,
			Fork:          r.Fork,
			Archived:      r.Archived,
			Description:   r.Description,
			DefaultBranch: r.DefaultBranch,
			SizeKB:        r.Size,
			UpdatedAt:     r.UpdatedAt,
			CloneURL:      r.CloneURL,
			LocalName:     GithubLocalName(q.NamePrefix, ownerLogin, r.Name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FetchGithubRepo 直接读取单个仓库的元数据（GET /repos/{owner}/{repo}）。
// 用于发现列表缺项时兜底：分页中断/列表接口瞬时不一致时仍能导入被勾选的仓库。
func FetchGithubRepo(q GithubDiscoverQuery, repo string) (*GithubRepo, error) {
	owner := strings.TrimSpace(q.Owner)
	if err := validateGithubOwner(owner); err != nil {
		return nil, err
	}
	repo = strings.TrimSpace(strings.TrimSuffix(repo, ".git"))
	if repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("invalid repository name %q", repo)
	}
	api := strings.TrimRight(strings.TrimSpace(q.APIBase), "/")
	if api == "" {
		api = defaultGithubAPIBase
	}
	client, err := newGithubClient(q.Proxy)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()

	body, _, err := githubRequest(client, fmt.Sprintf("%s/repos/%s/%s", api, url.PathEscape(owner), url.PathEscape(repo)), q.Token)
	if err != nil {
		return nil, err
	}
	var r githubAPIRepo
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: decode repo response: %v", ErrGithubUpstream, err)
	}
	ownerLogin := r.Owner.Login
	if ownerLogin == "" {
		ownerLogin = owner
	}
	return &GithubRepo{
		FullName:      r.FullName,
		Name:          r.Name,
		Owner:         ownerLogin,
		Private:       r.Private,
		Fork:          r.Fork,
		Archived:      r.Archived,
		Description:   r.Description,
		DefaultBranch: r.DefaultBranch,
		SizeKB:        r.Size,
		UpdatedAt:     r.UpdatedAt,
		CloneURL:      r.CloneURL,
		LocalName:     GithubLocalName(q.NamePrefix, ownerLogin, r.Name),
	}, nil
}

func validateGithubOwner(owner string) error {
	if strings.ContainsAny(owner, "/\\ \t") {
		return fmt.Errorf("invalid github owner %q", owner)
	}
	if strings.Contains(owner, "..") || strings.HasPrefix(owner, ".") {
		return fmt.Errorf("invalid github owner %q", owner)
	}
	return nil
}

// githubAPIRepo 是 GitHub API 仓库对象中用到的字段子集。
type githubAPIRepo struct {
	FullName      string                 `json:"full_name"`
	Name          string                 `json:"name"`
	Owner         struct{ Login string } `json:"owner"`
	Private       bool                   `json:"private"`
	Fork          bool                   `json:"fork"`
	Archived      bool                   `json:"archived"`
	Disabled      bool                   `json:"disabled"`
	Description   string                 `json:"description"`
	DefaultBranch string                 `json:"default_branch"`
	Size          int64                  `json:"size"` // KB
	UpdatedAt     string                 `json:"updated_at"`
	CloneURL      string                 `json:"clone_url"`
	SSHURL        string                 `json:"ssh_url"`
}

// GithubAPIError 是 GitHub API 的 HTTP 错误。
// 通过 Unwrap 暴露分类哨兵（ErrGithubNotFound / ErrGithubRateLimited），
// 同时保留 Status 供调用方判断「换一个入口重试」等场景（上层也据此回 502）。
type GithubAPIError struct {
	Status int
	msg    string
	err    error
}

func (e *GithubAPIError) Error() string { return e.msg }
func (e *GithubAPIError) Unwrap() error { return e.err }

func newGithubClient(proxy string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: githubRequestTimeout,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %v", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("proxy URL must be http or https, got %q", u.Scheme)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: transport, Timeout: githubRequestTimeout}, nil
}

func githubLogin(client *http.Client, api, token string) (string, error) {
	body, _, err := githubRequest(client, api+"/user", token)
	if err != nil {
		return "", err
	}
	var v struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("%w: decode /user response: %v", ErrGithubUpstream, err)
	}
	return v.Login, nil
}

// githubListRepos 拉取一个列表入口的全部页（Link 头 rel="next" 驱动）。
func githubListRepos(client *http.Client, endpoint, token string) ([]githubAPIRepo, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	query := u.Query()
	query.Set("per_page", fmt.Sprint(githubPerPage))
	query.Set("sort", "full_name")
	u.RawQuery = query.Encode()

	var out []githubAPIRepo
	next := u.String()
	for page := 0; page < githubMaxPages && next != ""; page++ {
		body, header, err := githubRequest(client, next, token)
		if err != nil {
			return nil, err
		}
		var items []githubAPIRepo
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("%w: decode repos page: %v", ErrGithubUpstream, err)
		}
		out = append(out, items...)
		next = parseGithubLinkNext(header.Get("Link"))
	}
	return out, nil
}

// parseGithubLinkNext 从 Link 头取 rel="next" 的 URL。
func parseGithubLinkNext(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, attr := range segs[1:] {
			if strings.Contains(attr, `rel="next"`) {
				return strings.TrimSuffix(strings.TrimPrefix(urlPart, "<"), ">")
			}
		}
	}
	return ""
}

// githubRequest 发起一次 GitHub API GET（带 Token、Accept 与版本头），
// 对网络错误与 5xx 做有限重试；返回响应体与响应头。
func githubRequest(client *http.Client, rawURL, token string) ([]byte, http.Header, error) {
	var lastErr error
	for attempt := 1; attempt <= githubRetryAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * time.Second)
		}
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "pgit")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%w: request failed: %v", ErrGithubUpstream, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("%w: read response: %v", ErrGithubUpstream, readErr)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return body, resp.Header, nil
		}
		he := classifyGithubStatus(resp, body)
		if resp.StatusCode >= 500 {
			lastErr = he
			continue // 5xx 可重试
		}
		return nil, nil, he
	}
	return nil, nil, lastErr
}

// classifyGithubStatus 把非 200 响应归类为哨兵错误（限流/未找到）或普通错误。
func classifyGithubStatus(resp *http.Response, body []byte) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return &GithubAPIError{Status: resp.StatusCode, msg: "github owner not found (404)", err: ErrGithubNotFound}
	case http.StatusUnauthorized:
		return &GithubAPIError{Status: resp.StatusCode, msg: "github: invalid token (401 unauthorized)"}
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("x-ratelimit-remaining") == "0" {
			reset := resp.Header.Get("x-ratelimit-reset")
			return &GithubAPIError{
				Status: resp.StatusCode,
				msg:    fmt.Sprintf("github api rate limit exceeded (reset at %s)", formatGithubReset(reset)),
				err:    ErrGithubRateLimited,
			}
		}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &GithubAPIError{Status: resp.StatusCode, msg: fmt.Sprintf("github: unexpected status %d: %s", resp.StatusCode, msg)}
}

// formatGithubReset 把 x-ratelimit-reset（Unix 秒）格式化成人可读时间。
func formatGithubReset(v string) string {
	var unix int64
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &unix); err != nil || unix == 0 {
		if v == "" {
			return "unknown"
		}
		return v
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}
