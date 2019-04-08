package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os/exec"

	"github.com/go-chi/chi"
)

type RepoDetail struct {
	Metadata *Repository `json:"metadata"`
	Files    []string    `json:"files"`
	Branch   []string    `json:"branch"`
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

func (handler RepoHandler) InfoRefs(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	repopath := RepositoryDir(repoName)
	service := r.FormValue("service")
	if len(service) > 0 {
		w.Header().Add("Content-type", fmt.Sprintf("application/x-%s-advertisement", service))
		cmd := exec.Command(
			"git",
			string(service[4:]),
			"--stateless-rpc",
			"--advertise-refs",
			repopath)
		cmd.Dir = repopath
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("error %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintln(w, "Internal Server Error")
			_, _ = w.Write(out)
		} else {
			serverAdvert := fmt.Sprintf("# service=%s", service)
			length := len(serverAdvert) + 4
			_, _ = fmt.Fprintf(w, "%04x%s0000", length, serverAdvert)
			_, _ = w.Write(out)
		}
	} else {
		_, _ = fmt.Fprintln(w, "Invalid request")
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (handler RepoHandler) Command(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	repopath := RepositoryDir(repoName)
	command := chi.URLParam(r, "command")
	if len(command) > 0 {

		w.Header().Add("Content-type", fmt.Sprintf("application/x-git-%s-result", command))
		w.WriteHeader(http.StatusOK)

		cmd := exec.Command("git", command, "--stateless-rpc", repopath)
		cmd.Dir = repopath

		cmdIn, _ := cmd.StdinPipe()
		cmdOut, _ := cmd.StdoutPipe()
		body := r.Body

		_ = cmd.Start()

		_, _ = io.Copy(cmdIn, body)
		_, _ = io.Copy(w, cmdOut)

		if command == "receive-pack" {
			updateCmd := exec.Command("git", "--git-dir", repopath, "update-server-info")
			_ = updateCmd.Start()
		}
	} else {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintln(w, "Invalid Request")
	}
}

func (handler RepoHandler) Create(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	description := r.FormValue("description")
	if _, exist := handler.Repositories[repoName]; exist {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "%s existed!", repoName)
	}
	repo := &Repository{
		Name:        repoName,
		Description: description,
	}
	err := repo.InitBare()
	if err == nil {
		handler.AddRepository(repo)
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, err.Error())
	}
}

func (handler RepoHandler) Delete(w http.ResponseWriter, r *http.Request) {
	confirm := r.FormValue("confirm")
	repoName := chi.URLParam(r, "repoName")
	repo, exist := handler.Repositories[repoName]
	if !exist {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "%s not existed!", repoName)
		return
	}

	if confirm != repoName {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "%s not confirm!", repoName)
		return
	}

	err := repo.Delete()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, err.Error())
		return
	}
	delete(handler.Repositories, repoName)
	w.WriteHeader(http.StatusOK)
}

func (handler RepoHandler) View(w http.ResponseWriter, r *http.Request) {

	repositories := make([]*Repository, 0)
	for _, repo := range handler.Repositories {
		repositories = append(repositories, repo)
	}

	output, err := json.Marshal(repositories)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(output)
}

func (handler RepoHandler) Detail(w http.ResponseWriter, r *http.Request) {

	repoName := chi.URLParam(r, "repoName")
	repo, exist := handler.Repositories[repoName]
	if !exist {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "%s not existed!", repoName)
		return
	}

	detail := RepoDetail{
		Metadata: repo,
	}

	output, err := json.Marshal(detail)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(output)
}

func (handler RepoHandler) Tree(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	branch := chi.URLParam(r, "branch")
	repo, exist := handler.Repositories[repoName]
	if !exist {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "%s not existed!", repoName)
		return
	}
	path := chi.URLParam(r, "*")

	files, err := repo.Tree(branch, path)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "error %v", err)
		return
	}
	output, _ := json.Marshal(files)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(output)
}

func (handler RepoHandler) Archive(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	branch := chi.URLParam(r, "branch")
	repo, exist := handler.Repositories[repoName]
	if !exist {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "%s not existed!", repoName)
		return
	}
	body, err := repo.Archive(branch)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}

	w.Header().Add("Content-type", "application/octet-stream")
	w.Header().Add("Content-Disposition", fmt.Sprintf("attachment; filename=%s.zip", repoName))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (handler RepoHandler) Blob(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repoName")
	branch := chi.URLParam(r, "branch")
	repo, exist := handler.Repositories[repoName]
	if !exist {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "%s not existed!", repoName)
		return
	}

	path := chi.URLParam(r, "*")
	if len(path) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Bad Request"))
		return
	}

	body, err := repo.Blob(branch, path)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(err.Error()))
		return
	}

	w.Header().Add("Content-type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (handler RepoHandler) Commit(w http.ResponseWriter, r *http.Request) {}

func (handler RepoHandler) Commits(w http.ResponseWriter, r *http.Request) {}

type DashboardResult struct {
	Total int `json:"total"`
}

func (handler RepoHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	dr := DashboardResult{
		Total: len(handler.Repositories),
	}
	output, _ := json.Marshal(dr)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(output)
}
