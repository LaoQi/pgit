package main

import (
	"encoding/json"
	"fmt"
	"io"
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

type RepoHandler struct {
	Credentials  map[string]string
	Repositories map[string]*Repository
}

func NewRepoHandler() *RepoHandler {
	r := &RepoHandler{
		Credentials: map[string]string{
			"test": "123456",
		},
		Repositories: map[string]*Repository{},
	}

	r.Explorer()

	return r
}

func (handler RepoHandler) Explorer() {
	root := GetSettings().GitRoot
	files, err := ioutil.ReadDir(root)
	if err != nil {
		panic(err)
	}
	for _, file := range files {
		if file.IsDir() {
			repo, err := CheckRepository(file.Name())
			if err == nil {
				handler.AddRepository(repo)
			} else {
				log.Print(err.Error())
			}
		}
	}
}

func (handler RepoHandler) AddRepository(repo *Repository) {
	handler.Repositories[repo.Name] = repo
	log.Printf("Add Repository %s", repo.Name)
}

func (handler RepoHandler) StaticFiles(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/repo")
	http.ServeFile(w, r, filepath.Join(GetSettings().GitRoot, path))
}

func (handler RepoHandler) InfoRefs(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	repopath := filepath.Join(GetSettings().GitRoot, repoName)
	service := r.FormValue("service")
	if len(service) > 0 {
		w.Header().Add("Content-type", fmt.Sprintf("application/x-%s-advertisement", service))
		gitLocalCmd := exec.Command(
			"git",
			string(service[4:]),
			"--stateless-rpc",
			"--advertise-refs",
			repopath)
		out, err := gitLocalCmd.CombinedOutput()
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintln(w, "Internal Server Error")
			w.Write(out)
		} else {
			serverAdvert := fmt.Sprintf("# service=%s", service)
			length := len(serverAdvert) + 4
			fmt.Fprintf(w, "%04x%s0000", length, serverAdvert)
			w.Write(out)
		}
	} else {
		fmt.Fprintln(w, "Invalid request")
		w.WriteHeader(400)
	}
}

func (handler RepoHandler) Command(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	repopath := filepath.Join(GetSettings().GitRoot, repoName)
	command := chi.URLParam(r, "command")
	if len(command) > 0 {

		w.Header().Add("Content-type", fmt.Sprintf("application/x-git-%s-result", command))
		w.WriteHeader(200)

		gitCmd := exec.Command("git", command, "--stateless-rpc", repopath)

		cmdIn, _ := gitCmd.StdinPipe()
		cmdOut, _ := gitCmd.StdoutPipe()
		body := r.Body

		gitCmd.Start()

		io.Copy(cmdIn, body)
		io.Copy(w, cmdOut)

		if command == "receive-pack" {
			updateCmd := exec.Command("git", "--git-dir", repopath, "update-server-info")
			updateCmd.Start()
		}
	} else {
		w.WriteHeader(400)
		fmt.Fprintln(w, "Invalid Request")
	}
}

func (handler RepoHandler) Create(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	repo := &Repository{
		Name: repoName,
	}
	err := repo.InitBare()
	if err != nil {
		handler.AddRepository(repo)
	}
}

func (handler RepoHandler) Delete(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (handler RepoHandler) View(w http.ResponseWriter, r *http.Request) {
	output, err := json.Marshal(handler.Repositories)
	if err != nil {
		w.WriteHeader(http.StatusExpectationFailed)
		w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(output)
}
