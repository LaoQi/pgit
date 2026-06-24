package pgs

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RepositoriesManagerConfig struct {
	GitRoot string
}

type RepositoriesManager struct {
	Config  *RepositoriesManagerConfig
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

func (r *RepositoriesManager) addRepository(repo *Repository) {
	r.byName[repo.Name] = repo
	for _, alias := range repo.Aliases {
		r.byAlias[alias] = repo
	}
}

func (r *RepositoriesManager) List() []*Repository {
	repos := make([]*Repository, 0, len(r.byName))
	for _, repo := range r.byName {
		repos = append(repos, repo)
	}
	return repos
}

func (r *RepositoriesManager) GetRepository(name string) (*Repository, error) {
	repo, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("repository %s not exist", name)
	}
	return repo, nil
}

func (r *RepositoriesManager) GetByAlias(alias string) (*Repository, error) {
	repo, ok := r.byAlias[alias]
	if !ok {
		return nil, fmt.Errorf("repository alias %s not exist", alias)
	}
	return repo, nil
}

func (r *RepositoriesManager) RepositoryExist(name string) bool {
	_, ok := r.byName[name]
	return ok
}

func (r *RepositoriesManager) CreateRepository(name string, description string) error {
	if err := ValidateRepoName(name); err != nil {
		return err
	}
	if r.RepositoryExist(name) {
		return fmt.Errorf("repository %s already exist", name)
	}
	repo, err := InitBare(name, description)
	if err != nil {
		return err
	}
	r.addRepository(repo)
	log.Printf("created repository %s", name)
	return nil
}

func (r *RepositoriesManager) DeleteRepository(name string) error {
	repo, err := r.GetRepository(name)
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
	if _, exist := r.byAlias[alias]; exist {
		return fmt.Errorf("alias %s already in use", alias)
	}
	repo, err := r.GetRepository(name)
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
	repo, err := r.GetRepository(name)
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
