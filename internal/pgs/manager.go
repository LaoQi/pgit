package pgs

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
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
}

var ReposManager *RepositoriesManager

func InitReposManager(config *RepositoriesManagerConfig) {
	if config.GitRoot != "" {
		GitRoot = config.GitRoot
	}
	ReposManager = &RepositoriesManager{
		Config:  config,
		byName:  map[string]*Repository{},
		byAlias: map[string]*Repository{},
	}
	ReposManager.CheckRepositories()
}

func (r *RepositoriesManager) CheckRepositories() {
	files, err := os.ReadDir(GitRoot)
	if err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, file := range files {
		if !file.IsDir() || !strings.HasSuffix(file.Name(), ".git") {
			continue
		}
		repo, err := r.loadRepo(file.Name())
		if err != nil {
			log.Printf("load repository %s failed: %v", file.Name(), err)
			continue
		}
		r.addRepository(repo)
	}
}

// loadRepo 读取仓库元数据（调用方须持有 r.mu）。
func (r *RepositoriesManager) loadRepo(dirName string) (*Repository, error) {
	repoDir := filepath.Join(GitRoot, dirName)
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
	}
	return &repo, nil
}

// migrateLegacyRepo 为缺 pgit.json 的旧仓库补元数据（调用方须持有 r.mu）。
func (r *RepositoriesManager) migrateLegacyRepo(dirName string) (*Repository, error) {
	name := strings.TrimSuffix(dirName, ".git")
	repoDir := filepath.Join(GitRoot, dirName)
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
	}
	if err := repo.SaveMetadata(); err != nil {
		log.Printf("migrate: write pgit.json for %s failed: %v", name, err)
	}
	log.Printf("migrated legacy repository %s, generated pgit.json", name)
	return repo, nil
}

// addRepository 写入双索引（调用方须持有 r.mu）。
func (r *RepositoriesManager) addRepository(repo *Repository) {
	r.byName[repo.Name] = repo
	for _, alias := range repo.Aliases {
		r.byAlias[alias] = repo
	}
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
		return nil, fmt.Errorf("repository %s not exist", name)
	}
	return repo, nil
}

// getByAliasLocked 按 alias 取内部指针（调用方须持有 r.mu）。
func (r *RepositoriesManager) getByAliasLocked(alias string) (*Repository, error) {
	repo, ok := r.byAlias[alias]
	if !ok {
		return nil, fmt.Errorf("repository alias %s not exist", alias)
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
		return fmt.Errorf("repository %s already exist", name)
	}
	repo, err := InitBare(name, description, defaultBranch)
	if err != nil {
		return err
	}
	r.addRepository(repo)
	log.Printf("created repository %s (default branch: %s)", name, defaultBranch)
	return nil
}

func (r *RepositoriesManager) CreateMirrorRepository(name string, description string, mirror *MirrorConfig) error {
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
		return fmt.Errorf("repository %s already exist", name)
	}
	repo, err := InitBare(name, description, "master")
	if err != nil {
		return err
	}
	repo.Mirror = &m
	if err := repo.SaveMetadata(); err != nil {
		return err
	}
	r.addRepository(repo)
	log.Printf("created mirror repository %s (remote: %s)", name, m.RemoteURL)
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
			return 0, fmt.Errorf("repository %s is not a mirror", name)
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
		return nil, fmt.Errorf("repository %s is not a mirror", name)
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

	result, fetchErr := git.FetchRemote(m.RemoteURL, repoPath, auth)

	r.mu.Lock()
	if repo, err := r.getByNameLocked(name); err == nil && repo.IsMirror() {
		repo.Mirror.LastSync = time.Now()
		if fetchErr != nil {
			repo.Mirror.LastError = fetchErr.Error()
		} else {
			repo.Mirror.LastError = ""
		}
		if err := repo.SaveMetadata(); err != nil {
			log.Printf("sync: save metadata for %s failed: %v", name, err)
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
	delete(r.byName, repo.Name)
	for _, alias := range repo.Aliases {
		delete(r.byAlias, alias)
	}
	log.Printf("deleted repository %s", name)
	return nil
}

func (r *RepositoriesManager) AddAlias(name string, alias string) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exist := r.byAlias[alias]; exist {
		return fmt.Errorf("alias %s already in use", alias)
	}
	repo, err := r.getByNameLocked(name)
	if err != nil {
		return err
	}
	if repo.HasAlias(alias) {
		return fmt.Errorf("alias %s already bound to repository %s", alias, name)
	}
	repo.Aliases = append(repo.Aliases, alias)
	r.byAlias[alias] = repo
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
	delete(r.byAlias, alias)
	return repo.SaveMetadata()
}

func ValidateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("repository name is empty")
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("repository name must not contain '/'")
	}
	if name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("repository name must not contain '..'")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("repository name must not start with '.'")
	}
	if name == "api" {
		return fmt.Errorf("repository name 'api' is reserved")
	}
	return nil
}

func ValidateAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("alias is empty")
	}
	if strings.HasPrefix(alias, "/") {
		return fmt.Errorf("alias must not start with '/'")
	}
	if strings.HasSuffix(alias, "/") {
		return fmt.Errorf("alias must not end with '/'")
	}
	if strings.Contains(alias, "//") {
		return fmt.Errorf("alias must not contain empty segment")
	}
	if strings.Contains(alias, "..") {
		return fmt.Errorf("alias must not contain '..'")
	}
	if strings.HasPrefix(alias, "api/") || alias == "api" {
		return fmt.Errorf("alias prefix 'api' is reserved")
	}
	return nil
}
