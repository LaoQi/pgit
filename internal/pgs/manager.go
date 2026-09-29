package pgs

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"pgit/internal/pgs/git"
)

type RepositoriesManagerConfig struct {
	GitRoot string
}

// RepositoriesManager 管理仓库索引（byName/byAlias）。
// 并发约定：
//   - 所有读写内部索引与仓库元数据的操作都必须持有 mu；
//   - 对外方法返回 Repository 的深拷贝快照（Snapshot），调用方拿到的是不可变值，
//     不会再与内部写入竞争；
//   - 元数据落盘（SaveMetadata）一律在持锁期间完成，避免「改动 + 覆盖写」交错丢更新。
type RepositoriesManager struct {
	Config  *RepositoriesManagerConfig
	mu      sync.RWMutex
	byName  map[string]*Repository
	byAlias map[string]*Repository
	// byRefFolded 是大小写折叠（小写）的 ref 索引，仅用于唯一性检测：
	// 解析仍走 byName/byAlias 的精确匹配，git 路径的大小写敏感语义不变。
	byRefFolded map[string]*Repository
	// namingConflicts 记录启动扫描发现的重名冲突（涉及的仓库已全部禁用）。
	namingConflicts []string
}

var ReposManager *RepositoriesManager

func InitReposManager(config *RepositoriesManagerConfig) {
	if config.GitRoot != "" {
		GitRoot = config.GitRoot // 过渡兜底，见 Repository.Root()
	}
	ReposManager = &RepositoriesManager{
		Config:      config,
		byName:      map[string]*Repository{},
		byAlias:     map[string]*Repository{},
		byRefFolded: map[string]*Repository{},
	}
	ReposManager.CheckRepositories()
}

// root 返回本 manager 使用的存储根目录。
func (r *RepositoriesManager) root() string {
	if r.Config != nil && r.Config.GitRoot != "" {
		return r.Config.GitRoot
	}
	return GitRoot // 过渡兜底
}

// CheckRepositories 扫描存储目录重建索引（仅初始化时调用一次：索引为累积写入，
// 重复调用不会清理已移除的仓库）。
//
// 唯一性策略（ref 空间 = 仓库名 ∪ 别名，大小写不敏感）：某个 ref 被多个仓库声明时，
// 涉及冲突的仓库**全部不进入索引**（即全部不可用），并逐条记 ERROR 日志等待人工修复；
// 同仓库内部重复的 ref 只记 WARN 并去重。
func (r *RepositoriesManager) CheckRepositories() {
	files, err := os.ReadDir(r.root())
	if err != nil {
		panic(err)
	}

	loaded := make([]*Repository, 0, len(files))
	for _, file := range files {
		if !file.IsDir() || !strings.HasSuffix(file.Name(), ".git") {
			continue
		}
		repo, err := r.loadRepo(file.Name())
		if err != nil {
			slog.Error("load repository failed", "dir", file.Name(), "error", err)
			continue
		}
		loaded = append(loaded, repo)
	}
	// 输出顺序稳定，便于日志比对与测试。
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Name < loaded[j].Name })

	owners := map[string][]*Repository{}
	for _, repo := range loaded {
		seen := map[string]bool{}
		for _, ref := range refsOf(repo) {
			k := refFolded(ref)
			if seen[k] {
				slog.Warn("duplicate ref in repository metadata", "repo", repo.Name, "ref", ref)
				continue
			}
			seen[k] = true
			owners[k] = append(owners[k], repo)
		}
	}

	keys := make([]string, 0, len(owners))
	for k := range owners {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	conflicted := map[string]bool{}
	conflicts := make([]string, 0)
	for _, k := range keys {
		group := owners[k]
		if len(group) < 2 {
			continue
		}
		names := make([]string, 0, len(group))
		for _, repo := range group {
			conflicted[repo.Name] = true
			names = append(names, repo.Name)
		}
		sort.Strings(names)
		conflicts = append(conflicts, fmt.Sprintf("ref %q claimed by repositories: %s", k, strings.Join(names, ", ")))
		slog.Error("naming conflict: repositories disabled", "ref", k, "repos", strings.Join(names, ", "))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.namingConflicts = conflicts
	for _, repo := range loaded {
		if conflicted[repo.Name] {
			continue
		}
		r.addRepository(repo)
	}
	if len(conflicts) > 0 {
		slog.Error("repositories disabled due to naming conflicts",
			"conflicts", len(conflicts), "disabled", len(conflicted))
	}
}

// loadRepo 读取仓库元数据（调用方须持有 r.mu）。
func (r *RepositoriesManager) loadRepo(dirName string) (*Repository, error) {
	repoDir := filepath.Join(r.root(), dirName)
	metaPath := filepath.Join(repoDir, "pgit.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return r.migrateLegacyRepo(dirName)
		}
		return nil, err
	}
	var repo Repository
	if err := json.Unmarshal(data, &repo); err != nil {
		return nil, err
	}
	if repo.Name == "" {
		repo.Name = strings.TrimSuffix(dirName, ".git")
	}
	if len(repo.Aliases) == 0 {
		repo.Aliases = []string{repo.Name}
	} else if !repo.HasAlias(repo.Name) {
		// 仓库名永远是一个 ref（git 传输只按 ref 解析），元数据缺失时在内存补齐。
		slog.Warn("repository metadata missing name ref; registered in memory", "repo", repo.Name)
		repo.Aliases = append([]string{repo.Name}, repo.Aliases...)
	}
	repo.root = r.root()
	return &repo, nil
}

// migrateLegacyRepo 为缺 pgit.json 的旧仓库补元数据（调用方须持有 r.mu）。
func (r *RepositoriesManager) migrateLegacyRepo(dirName string) (*Repository, error) {
	name := strings.TrimSuffix(dirName, ".git")
	repoDir := filepath.Join(r.root(), dirName)
	descData, err := os.ReadFile(filepath.Join(repoDir, "description"))
	if err != nil {
		return nil, err
	}
	description := strings.TrimPrefix(string(descData), fmt.Sprintf("%s;", name))
	repo := &Repository{
		Name:        name,
		Description: description,
		Aliases:     []string{name},
		CreatedAt:   time.Now(),
		root:        r.root(),
	}
	if err := repo.SaveMetadata(); err != nil {
		slog.Error("migrate write metadata failed", "repo", name, "error", err)
	}
	slog.Info("migrated legacy repository", "repo", name)
	return repo, nil
}

// addRepository 写入双索引（调用方须持有 r.mu）。
// refsOf 返回仓库占用的全部 ref（仓库名优先，随后是别名）。
func refsOf(repo *Repository) []string {
	refs := make([]string, 0, len(repo.Aliases)+1)
	refs = append(refs, repo.Name)
	for _, a := range repo.Aliases {
		if a != repo.Name {
			refs = append(refs, a)
		}
	}
	return refs
}

// refFolded 返回唯一性比较用的大小写折叠键（解析仍按原始值精确匹配）。
func refFolded(ref string) string { return strings.ToLower(ref) }

// addRepository 把仓库的全部 ref 写入索引（调用方须持有 r.mu）。
func (r *RepositoriesManager) addRepository(repo *Repository) {
	r.byName[repo.Name] = repo
	for _, ref := range refsOf(repo) {
		r.byAlias[ref] = repo
		r.byRefFolded[refFolded(ref)] = repo
	}
}

// unregisterRefLocked 从索引移除仓库的全部 ref（调用方须持有 r.mu）。
func (r *RepositoriesManager) unregisterRefLocked(repo *Repository) {
	delete(r.byName, repo.Name)
	for _, ref := range refsOf(repo) {
		if cur, ok := r.byAlias[ref]; ok && cur.Name == repo.Name {
			delete(r.byAlias, ref)
		}
		k := refFolded(ref)
		if cur, ok := r.byRefFolded[k]; ok && cur.Name == repo.Name {
			delete(r.byRefFolded, k)
		}
	}
}

// checkRefAvailableLocked 检查 ref 是否尚未被任何仓库占用（调用方须持有 r.mu）。
func (r *RepositoriesManager) checkRefAvailableLocked(ref string) error {
	if repo, ok := r.byRefFolded[refFolded(ref)]; ok {
		return fmt.Errorf("%w: ref %q is already used by repository %q", ErrRefConflict, ref, repo.Name)
	}
	return nil
}

// List 返回全部仓库的元数据快照。
func (r *RepositoriesManager) List() []*Repository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	repos := make([]*Repository, 0, len(r.byName))
	for _, repo := range r.byName {
		repos = append(repos, repo.Snapshot())
	}
	return repos
}

// GetRepository 按 name 返回元数据快照。
func (r *RepositoriesManager) GetRepository(name string) (*Repository, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	repo, err := r.getByNameLocked(name)
	if err != nil {
		return nil, err
	}
	return repo.Snapshot(), nil
}

// GetByAlias 按 git 访问 alias 返回元数据快照。
func (r *RepositoriesManager) GetByAlias(alias string) (*Repository, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	repo, err := r.getByAliasLocked(alias)
	if err != nil {
		return nil, err
	}
	return repo.Snapshot(), nil
}

// Resolve 把 ref（仓库名或别名）解析为仓库元数据快照。
//
// 解析按原始值精确匹配（git 路径大小写敏感），不做大小写折叠；未命中返回 ErrRepoNotFound。
func (r *RepositoriesManager) Resolve(ref string) (*Repository, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if repo, ok := r.byName[ref]; ok {
		return repo.Snapshot(), nil
	}
	if repo, ok := r.byAlias[ref]; ok {
		return repo.Snapshot(), nil
	}
	return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, ref)
}

// NamingConflicts 返回启动扫描发现的重名冲突描述（只读快照）。
func (r *RepositoriesManager) NamingConflicts() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.namingConflicts...)
}

// RepositoryExist 判断 name 是否已存在。
func (r *RepositoriesManager) RepositoryExist(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.repoExistsLocked(name)
}

// getByNameLocked 按 name 取内部指针（调用方须持有 r.mu）。
func (r *RepositoriesManager) getByNameLocked(name string) (*Repository, error) {
	repo, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, name)
	}
	return repo, nil
}

// getByAliasLocked 按 alias 取内部指针（调用方须持有 r.mu）。
func (r *RepositoriesManager) getByAliasLocked(alias string) (*Repository, error) {
	repo, ok := r.byAlias[alias]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAliasNotFound, alias)
	}
	return repo, nil
}

// repoExistsLocked 判断 name 是否存在（调用方须持有 r.mu）。
func (r *RepositoriesManager) repoExistsLocked(name string) bool {
	_, ok := r.byName[name]
	return ok
}

func (r *RepositoriesManager) CreateRepository(name string, description string, defaultBranch string) error {
	if err := ValidateRepoName(name); err != nil {
		return err
	}
	if defaultBranch == "" {
		defaultBranch = "master"
	}
	if err := ValidateDefaultBranch(defaultBranch); err != nil {
		return fmt.Errorf("invalid default branch: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repoExistsLocked(name) {
		return fmt.Errorf("%w: %s", ErrRepoExist, name)
	}
	if err := r.checkRefAvailableLocked(name); err != nil {
		return err
	}
	repo, err := InitBare(r.root(), name, description, defaultBranch)
	if err != nil {
		return err // InitBare 内部已回滚半成品目录
	}
	r.addRepository(repo)
	slog.Info("created repository", "repo", name, "defaultBranch", defaultBranch)
	return nil
}

func (r *RepositoriesManager) CreateMirrorRepository(name string, description string, mirror *MirrorConfig) error {
	return r.CreateMirrorRepositoryWithBranch(name, description, mirror, "")
}

// CreateMirrorRepositoryWithBranch 与 CreateMirrorRepository 相同，但可指定默认分支
// （HEAD 初始指向，如 GitHub 仓库的 default_branch；空值默认 master）。
func (r *RepositoriesManager) CreateMirrorRepositoryWithBranch(name string, description string, mirror *MirrorConfig, defaultBranch string) error {
	if err := ValidateRepoName(name); err != nil {
		return err
	}
	if mirror == nil {
		return fmt.Errorf("mirror config is nil")
	}
	m := *mirror
	if err := validateMirror(&m); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repoExistsLocked(name) {
		return fmt.Errorf("%w: %s", ErrRepoExist, name)
	}
	if err := r.checkRefAvailableLocked(name); err != nil {
		return err
	}
	repo, err := InitBare(r.root(), name, description, defaultBranch)
	if err != nil {
		return err
	}
	repo.Mirror = &m
	if err := repo.SaveMetadata(); err != nil {
		return err
	}
	r.addRepository(repo)
	slog.Info("created mirror repository", "repo", name, "remote", m.RemoteURL)
	return nil
}

// validateMirror 校验镜像配置（RemoteURL scheme/Proxy scheme/SyncInterval/AuthType）。
func validateMirror(m *MirrorConfig) error {
	if m.RemoteURL == "" {
		return fmt.Errorf("mirror remote URL is empty")
	}
	u, err := url.Parse(m.RemoteURL)
	if err != nil {
		return fmt.Errorf("invalid mirror remote URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("mirror remote URL must be http or https, got %q", u.Scheme)
	}
	if m.SyncInterval < 0 {
		return fmt.Errorf("mirror sync interval must be >= 0")
	}
	if m.Proxy != "" {
		proxyURL, err := url.Parse(m.Proxy)
		if err != nil {
			return fmt.Errorf("invalid mirror proxy URL: %v", err)
		}
		if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
			return fmt.Errorf("mirror proxy URL must be http or https, got %q", proxyURL.Scheme)
		}
	}
	if m.AuthType == "" {
		m.AuthType = "none"
	}
	return nil
}

// UpdateRepositorySettings 更新仓库描述与镜像配置，返回更新前的 SyncInterval。
// mirror 为 nil 时仅更新 description；镜像仓库传 mirror 时全量覆盖镜像字段，
// Password 为空表示保留原密码。整个更新（含落盘）在锁内完成。
func (r *RepositoriesManager) UpdateRepositorySettings(name string, description string, mirror *MirrorConfig) (int, error) {
	// 在副本上校验，避免修改调用方传入的对象（validateMirror 会补默认 AuthType）
	var updates *MirrorConfig
	if mirror != nil {
		cp := *mirror
		if err := validateMirror(&cp); err != nil {
			return 0, err
		}
		updates = &cp
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	repo, err := r.getByNameLocked(name)
	if err != nil {
		return 0, err
	}
	oldInterval := 0
	if repo.IsMirror() {
		oldInterval = repo.Mirror.SyncInterval
	}
	if updates != nil {
		if !repo.IsMirror() {
			return 0, fmt.Errorf("%w: %s", ErrNotMirror, name)
		}
		m := *updates
		if m.Password == "" {
			m.Password = repo.Mirror.Password
		}
		m.LastSync = repo.Mirror.LastSync
		m.LastError = repo.Mirror.LastError
		repo.Mirror = &m
	}
	repo.Description = description
	return oldInterval, repo.SaveMetadata()
}

// SyncRepository 执行一次镜像同步：锁内取配置快照 → 无锁 fetch → 锁内回写状态。
func (r *RepositoriesManager) SyncRepository(name string) (*git.FetchResult, error) {
	r.mu.RLock()
	repo, err := r.getByNameLocked(name)
	if err != nil {
		r.mu.RUnlock()
		return nil, err
	}
	if !repo.IsMirror() {
		r.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrNotMirror, name)
	}
	m := *repo.Mirror
	repoPath := repo.Path()
	r.mu.RUnlock()

	var auth *git.FetchAuth
	if m.AuthType == "basic" || m.Proxy != "" {
		auth = &git.FetchAuth{
			Type:     m.AuthType,
			Username: m.Username,
			Password: m.Password,
			Proxy:    m.Proxy,
		}
	}

	// 超时/重试策略来自全局配置（可经 SIGHUP 热加载）
	result, fetchErr := git.FetchRemoteWithOptions(m.RemoteURL, repoPath, auth, Settings.MirrorFetchOptions())

	r.mu.Lock()
	if repo, err := r.getByNameLocked(name); err == nil && repo.IsMirror() {
		repo.Mirror.LastSync = time.Now()
		if fetchErr != nil {
			repo.Mirror.LastError = fetchErr.Error()
		} else {
			repo.Mirror.LastError = ""
		}
		if err := repo.SaveMetadata(); err != nil {
			slog.Error("sync save metadata failed", "repo", name, "error", err)
		}
	}
	r.mu.Unlock()
	return result, fetchErr
}

func (r *RepositoriesManager) DeleteRepository(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	repo, err := r.getByNameLocked(name)
	if err != nil {
		return err
	}
	if err := repo.Delete(); err != nil {
		return err
	}
	r.unregisterRefLocked(repo)
	slog.Info("deleted repository", "repo", name)
	return nil
}

func (r *RepositoriesManager) AddAlias(name string, alias string) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	repo, err := r.getByNameLocked(name)
	if err != nil {
		return err
	}
	if repo.HasAlias(alias) {
		// 同一仓库重复绑定也归为 ref 冲突（HTTP 层统一 409）。
		return fmt.Errorf("%w: ref %q is already bound to repository %q", ErrRefConflict, alias, name)
	}
	// ref 空间唯一：别名不得与任何仓库名或别名（含大小写变体）冲突。
	if err := r.checkRefAvailableLocked(alias); err != nil {
		return err
	}
	repo.Aliases = append(repo.Aliases, alias)
	r.byAlias[alias] = repo
	r.byRefFolded[refFolded(alias)] = repo
	return repo.SaveMetadata()
}

func (r *RepositoriesManager) RemoveAlias(name string, alias string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	repo, err := r.getByNameLocked(name)
	if err != nil {
		return err
	}
	if alias == repo.Name {
		return fmt.Errorf("cannot remove default alias (repository name)")
	}
	if !repo.HasAlias(alias) {
		return fmt.Errorf("alias %s not bound to repository %s", alias, name)
	}
	newAliases := make([]string, 0, len(repo.Aliases)-1)
	for _, a := range repo.Aliases {
		if a != alias {
			newAliases = append(newAliases, a)
		}
	}
	repo.Aliases = newAliases
	if cur, ok := r.byAlias[alias]; ok && cur.Name == repo.Name {
		delete(r.byAlias, alias)
	}
	k := refFolded(alias)
	if cur, ok := r.byRefFolded[k]; ok && cur.Name == repo.Name {
		delete(r.byRefFolded, k)
	}
	return repo.SaveMetadata()
}

// ref 规则：仓库名与别名共用同一套字符与长度约束，区别只在段数。
//
// 约束目标是「零编码往返」：ref 必须能直接出现在 URL 路径、查询参数与表单值中而
// 无需百分号编码，也不会被 net/http 的路径归一化改写。
const (
	refMaxLen        = 100 // 整个 ref 的最大长度（字节）
	refSegmentMaxLen = 64  // 单个段的最大长度（字节）
	refMaxSegments   = 8   // 别名的最大段数（仓库名固定 1 段）
)

// refSegmentPattern 限定段的合法形态：首尾必须是字母/数字/下划线，中间可含 . - _。
// 由此天然排除空段、"."、".."、段首尾的点与连字符，以及所有需要 URL 编码的字符。
var refSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9._-]*[A-Za-z0-9_])?$`)

// exactReservedRefs 是精确保留的 ref（不区分大小写）：探针端点。
var exactReservedRefs = []string{"healthz", "metrics"}

// prefixReservedRefs 返回前缀保留清单（含实时读取的 WebUI 前缀）。
//
// /api/ 与 /{webuiPrefix}/ 的路由模式比 "/" 兜底更具体，同名 ref 会被遮蔽而永远不可达。
func prefixReservedRefs() []string {
	reserved := []string{"api"}
	if Settings != nil {
		if prefix, _ := Settings.WebUIConf(); strings.Trim(prefix, "/") != "" {
			reserved = append(reserved, strings.Trim(prefix, "/"))
		}
	}
	return reserved
}

// IsReservedRef 判断 ref 是否与系统路由保留字冲突（不区分大小写）。
func IsReservedRef(ref string) bool {
	low := strings.ToLower(strings.Trim(ref, "/"))
	if low == "" {
		return false
	}
	for _, r := range exactReservedRefs {
		if low == r {
			return true
		}
	}
	for _, r := range prefixReservedRefs() {
		r = strings.ToLower(r)
		if low == r || strings.HasPrefix(low, r+"/") {
			return true
		}
	}
	return false
}

// validateRefShape 校验 ref 的段结构（字符集、长度、段数、.git 结尾）。
func validateRefShape(ref string, maxSegments int) error {
	if ref == "" {
		return fmt.Errorf("ref is empty")
	}
	if len(ref) > refMaxLen {
		return fmt.Errorf("ref must be at most %d bytes", refMaxLen)
	}
	if strings.HasSuffix(strings.ToLower(ref), ".git") {
		return fmt.Errorf("ref must not end with '.git'")
	}
	segments := strings.Split(ref, "/")
	if len(segments) > maxSegments {
		return fmt.Errorf("ref must have at most %d segments", maxSegments)
	}
	for _, seg := range segments {
		if seg == "" {
			return fmt.Errorf("ref must not contain empty segment")
		}
		if len(seg) > refSegmentMaxLen {
			return fmt.Errorf("ref segment %q must be at most %d bytes", seg, refSegmentMaxLen)
		}
		if !refSegmentPattern.MatchString(seg) {
			return fmt.Errorf("ref segment %q is invalid: allowed chars are A-Za-z0-9_-. with alphanumeric ends", seg)
		}
	}
	return nil
}

// ValidateRepoName 校验仓库名（唯一标识，单段 ref）。
func ValidateRepoName(name string) error {
	if strings.Contains(name, "/") {
		return fmt.Errorf("repository name must not contain '/'")
	}
	if err := validateRefShape(name, 1); err != nil {
		return err
	}
	if IsReservedRef(name) {
		return fmt.Errorf("repository name %q is reserved", name)
	}
	return nil
}

// ValidateAlias 校验别名（多段 ref）。
func ValidateAlias(alias string) error {
	if err := validateRefShape(alias, refMaxSegments); err != nil {
		return err
	}
	if IsReservedRef(alias) {
		return fmt.Errorf("alias %q is reserved", alias)
	}
	return nil
}
