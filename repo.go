package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi"
)

type Repository struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (repo Repository) InitBare() error {
	repopath := RepositoryDir(repo.Name)
	_, err := os.Stat(repopath)
	if os.IsExist(err) {
		return fmt.Errorf("Cannot init repository %s, Directory exist!", repo.Name)
	}
	gitInitCmd := exec.Command("git", "init", "--bare", repopath)
	_, err = gitInitCmd.CombinedOutput()
	if err != nil {
		return err
	}

	return nil
}

func CheckRepository(name string) (*Repository, error) {
	if !IsRepositoryDir(name) {
		return nil, fmt.Errorf("%s Not Repository directory", name)
	}
	description, err := ioutil.ReadFile(filepath.Join(GetSettings().GitRoot, name, "description"))
	if err != nil {
		return nil, err
	}

	// load metadata
	repo := &Repository{
		Name:        strings.TrimSuffix(name, ".git"),
		Description: string(description),
	}
	return repo, nil
}

func RepositoryDir(name string) string {
	return filepath.Join(GetSettings().GitRoot, fmt.Sprintf("%s.git", name))
}

func IsRepositoryDir(name string) bool {
	if !strings.HasSuffix(name, ".git") {
		return false
	}

	_, err := os.Stat(filepath.Join(GetSettings().GitRoot, name, "description"))
	if os.IsNotExist(err) {
		return false
	}

	return true
}

type RepoApi struct {
	Credentials  map[string]string
	Repositories map[string]*Repository
	Router       chi.Router
}

func NewRepoApi() *RepoApi {
	r := chi.NewRouter()

	repoApi := &RepoApi{
		Credentials: map[string]string{
			"test": "123456",
		},
		Router:       r,
		Repositories: map[string]*Repository{},
	}

	r.Get("/", repoApi.View)
	r.Post("/{repoName}", repoApi.Create)
	repoApi.Explorer()
	return repoApi
}

func (repoApi RepoApi) Explorer() {
	root := GetSettings().GitRoot
	files, err := ioutil.ReadDir(root)
	if err != nil {
		panic(err)
	}
	for _, file := range files {
		if file.IsDir() {
			repo, err := CheckRepository(file.Name())
			if err == nil {
				repoApi.AddRepository(repo)
			} else {
				log.Print(err.Error())
			}
		}
	}
}

func (repoApi RepoApi) AddRepository(repo *Repository) {
	repoApi.Repositories[repo.Name] = repo

	// handler := http.FileServer(http.Dir(RepositoryDir(repo.Name)))
	// path, _ := os.Getwd()
	// handler := http.FileServer(http.Dir(path))
	// repoApi.Router.HandleFunc(fmt.Sprintf("/%s.git/*", repo.Name), handler.ServeHTTP)
	// repoApi.Router.HandleFunc(fmt.Sprintf("/%s.git/*", repo.Name), handler.ServeHTTP)
	repoApi.Router.HandleFunc(fmt.Sprintf("/%s.git/*", repo.Name), func(w http.ResponseWriter, r *http.Request) {
		upath := r.URL.Path
		p1 := strings.TrimPrefix(upath, "/repo")
		http.ServeFile(w, r, filepath.Join(GetSettings().GitRoot, p1))
	})
	log.Printf("Add Repository %s", repo.Name)
}

func (repoApi RepoApi) GetRouters() chi.Router {
	return repoApi.Router
}

func (repoApi RepoApi) Create(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	repo := &Repository{
		Name: repoName,
	}
	err := repo.InitBare()
	if err != nil {
		repoApi.AddRepository(repo)
	}
}

func (repoApi RepoApi) Delete(w http.ResponseWriter, r *http.Request) {

}

func (repoApi RepoApi) View(w http.ResponseWriter, r *http.Request) {
	output, err := json.Marshal(repoApi.Repositories)
	if err != nil {
		w.WriteHeader(http.StatusExpectationFailed)
		w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(output)
}
