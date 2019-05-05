package main

import (
	"fmt"
	"io/ioutil"
	"log"
)

type RepositoriesManager struct {
	Repositories map[string]*Repository
}

var ReposManager *RepositoriesManager

func InitReposManager() {
	ReposManager = &RepositoriesManager{
		Repositories: map[string]*Repository{},
	}
	ReposManager.CheckRepositories()
}

func (r *RepositoriesManager) CheckRepositories() {
	root := Settings.GitRoot
	files, err := ioutil.ReadDir(root)
	if err != nil {
		panic(err)
	}
	for _, file := range files {
		if file.IsDir() {
			repo, err := CheckRepository(file.Name())
			if err == nil {
				r.AddRepository(repo)
			} else {
				// log.Print(err.Error())
			}
		}
	}
}

func (r *RepositoriesManager) AddRepository(repo *Repository) {
	r.Repositories[repo.Name] = repo
	log.Printf("Add Repository %s", repo.Name)
}

func (r *RepositoriesManager) GetRepository(name string) (*Repository, error) {
	if _, exist := r.Repositories[name]; exist {
		return r.Repositories[name], nil
	}
	return nil, fmt.Errorf("Repository %s not exist!", name)
}

func (r *RepositoriesManager) RepositoryExist(name string) bool {
	if _, exist := r.Repositories[name]; exist {
		return true
	}
	return false
}

func (r *RepositoriesManager) CreateRepository(repo *Repository) error {
	err := repo.InitBare()
	if err != nil {
		return err
	}
	r.AddRepository(repo)
	return nil
}

func (r *RepositoriesManager) DeleteRepository(name string) error {
	repo, err := r.GetRepository(name)
	if err != nil {
		return err
	}
	err = repo.Delete()
	if err != nil {
		return err
	}
	delete(r.Repositories, name)
	return nil
}
