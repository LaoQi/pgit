package server

import "net/http"

type apiDocParam struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Example  string `json:"example"`
	Desc     string `json:"desc"`
}

type apiDocEndpoint struct {
	Method          string        `json:"method"`
	Path            string        `json:"path"`
	Summary         string        `json:"summary"`
	Params          []apiDocParam `json:"params"`
	RequestExample  string        `json:"requestExample"`
	ResponseExample string        `json:"responseExample"`
	Curl            string        `json:"curl"`
	Notes           []string      `json:"notes"`
}

type apiDocData struct {
	Title     string          `json:"title"`
	Endpoints []apiDocEndpoint `json:"endpoints"`
}

var apiDocs = apiDocData{
	Title: "pgit Management API",
	Endpoints: []apiDocEndpoint{
		{
			Method:  "GET",
			Path:    "/api/v1/repos",
			Summary:  "List all repositories",
			ResponseExample: `{
  "total": 1,
  "repositories": [
    {
      "name": "my-repo",
      "description": "A demo repository",
      "aliases": ["my-repo"],
      "createdAt": "2026-06-24T10:00:00Z"
    }
  ]
}`,
			Curl: "curl http://localhost:3000/api/v1/repos",
			Notes: []string{
				"Response is an object with total and repositories array, not a bare array.",
				"Repository order is not guaranteed (map iteration).",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/repos/{name}",
			Summary:  "Create a new bare repository",
 			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name, cannot contain /, .., or start with ."},
				{Name: "description", In: "form", Required: false, Example: "A demo repo", Desc: "Form field, NOT JSON body"},
				{Name: "defaultBranch", In: "form", Required: false, Example: "main", Desc: "Default branch name (initial HEAD), defaults to master"},
			},
			RequestExample: `POST /api/v1/repos/my-repo HTTP/1.1
Content-Type: application/x-www-form-urlencoded

description=A%20demo%20repo&defaultBranch=main`,
			ResponseExample: `{
  "name": "my-repo",
  "description": "A demo repo",
  "aliases": ["my-repo"],
  "createdAt": "2026-06-24T10:00:00Z"
}`,
 			Curl: `curl -X POST http://localhost:3000/api/v1/repos/my-repo \
  -d "description=A demo repo" \
  -d "defaultBranch=main"`,
			Notes: []string{
				"description is a form field (application/x-www-form-urlencoded), NOT a JSON body.",
				"defaultBranch sets the initial HEAD symref target; defaults to 'master' if omitted.",
				"name is auto-added as the first alias.",
				"Returns the full Repository object on success.",
				"Name validation: no /, no .., no leading dot, cannot be 'api'.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/{name}",
			Summary:  "Get repository metadata and refs (branches/tags)",
			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
			},
			ResponseExample: `{
  "metadata": {
    "name": "my-repo",
    "description": "A demo repo",
    "aliases": ["my-repo"],
    "createdAt": "2026-06-24T10:00:00Z"
  },
  "refs": [
    {
      "type": "commit",
      "name": "master",
      "author": "LaoQi",
      "email": "q@example.com",
      "timestamp": 1719216000,
      "subject": "initial commit"
    }
  ]
}`,
			Curl: "curl http://localhost:3000/api/v1/repos/my-repo",
			Notes: []string{
				"Returns both metadata and refs in one response.",
				"refs includes branches (type=commit) and tags (type=tag).",
				"Empty repository returns refs as empty array [].",
				"This is the only way to get the list of branches/tags.",
			},
		},
		{
			Method:  "DELETE",
			Path:    "/api/v1/repos/{name}",
			Summary:  "Delete a repository permanently",
			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
				{Name: "confirm", In: "query", Required: true, Example: "my-repo", Desc: "Must equal the repository name"},
			},
			RequestExample: `DELETE /api/v1/repos/my-repo?confirm=my-repo HTTP/1.1`,
			Curl: `curl -X DELETE "http://localhost:3000/api/v1/repos/my-repo?confirm=my-repo"`,
			Notes: []string{
				"confirm query parameter must match the repository name exactly.",
				"Returns empty body with 200 on success.",
				"This action is irreversible and deletes all git data.",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/repos/{name}/aliases",
			Summary:  "Add a new alias to a repository",
			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
				{Name: "alias", In: "form", Required: true, Example: "group/repo", Desc: "Form field, can contain slashes"},
			},
			RequestExample: `POST /api/v1/repos/my-repo/aliases HTTP/1.1
Content-Type: application/x-www-form-urlencoded

alias=group%2Frepo`,
			ResponseExample: `{
  "name": "my-repo",
  "description": "A demo repo",
  "aliases": ["my-repo", "group/repo"],
  "createdAt": "2026-06-24T10:00:00Z"
}`,
			Curl: `curl -X POST http://localhost:3000/api/v1/repos/my-repo/aliases \
  -d "alias=group/repo"`,
			Notes: []string{
				"alias is a form field, NOT JSON body.",
				"Alias can contain slashes (e.g. group/repo) for nested paths.",
				"Alias validation: no leading/trailing /, no //, no .., cannot be 'api'.",
				"Returns the updated full Repository object.",
			},
		},
		{
			Method:  "DELETE",
			Path:    "/api/v1/repos/{name}/aliases/{alias}",
			Summary:  "Remove an alias from a repository",
			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
				{Name: "alias", In: "path", Required: true, Example: "group/repo", Desc: "Alias to remove (single segment only)"},
			},
			ResponseExample: `{
  "name": "my-repo",
  "description": "A demo repo",
  "aliases": ["my-repo"],
  "createdAt": "2026-06-24T10:00:00Z"
}`,
			Curl: `curl -X DELETE http://localhost:3000/api/v1/repos/my-repo/aliases/group%2Frepo`,
			Notes: []string{
				"The default alias (same as repo name) cannot be removed.",
				"Route {alias} is a single path segment - aliases containing slashes cannot be deleted via this endpoint.",
				"Returns the updated full Repository object on success.",
 			},
 		},
 		{
 			Method:  "POST",
 			Path:    "/api/v1/repos/{name}/default-branch",
 			Summary:  "Set repository default branch (HEAD symref)",
 			Params: []apiDocParam{
 				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
 				{Name: "branch", In: "form", Required: true, Example: "main", Desc: "Short branch name (e.g. 'main'), must already exist in refs/heads/"},
 			},
 			RequestExample: `POST /api/v1/repos/my-repo/default-branch HTTP/1.1
Content-Type: application/x-www-form-urlencoded

branch=main`,
 			ResponseExample: `{
   "ok": true,
   "defaultBranch": "main"
}`,
 			Curl: `curl -X POST http://localhost:3000/api/v1/repos/my-repo/default-branch \
  -d "branch=main"`,
 			Notes: []string{
 				"Requires the branch to already exist (refs/heads/<branch> must have an oid).",
 				"Changes the HEAD symref atomically (lock+rename).",
 				"Browsing API (tree/blob/archive) without ref uses this default.",
 				"Does not modify git data, only changes which branch is checked out on clone.",
 			},
 		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/{name}/tree/{ref}/*",
			Summary:  "Browse repository tree (directory listing)",
 			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
				{Name: "ref", In: "path", Required: false, Example: "master", Desc: "Branch name, tag name, or commit OID. Defaults to repository default branch"},
				{Name: "*", In: "path", Required: false, Example: "src/pkg", Desc: "Subdirectory path within the tree"},
			},
			ResponseExample: `[
   {"type": "tree", "hash": "4e6f77e...", "name": "src"},
   {"type": "blob", "hash": "a1b2c3d...", "name": "README.md"},
   {"type": "commit", "hash": "deadbef...", "name": "vendor/submodule"}
]`,
			Curl: `curl http://localhost:3000/api/v1/repos/my-repo/tree/master/src`,
			Notes: []string{
				"Response is a bare JSON array, not wrapped in an object.",
				"ref supports: branch name, tag name, 'HEAD', or full 40-char commit OID.",
				"Default ref is repository's default branch (HEAD symref target), not hard-coded 'master'.",
				"type 'tree' = directory, 'blob' = file, 'commit' = gitlink/submodule.",
				"Empty repository returns 400 (ref not found).",
				"Use GET /api/v1/repos/{name} to list available refs.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/{name}/blob/{ref}/*",
			Summary:  "Read file content (blob)",
 			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
				{Name: "ref", In: "path", Required: false, Example: "master", Desc: "Branch name, tag name, or commit OID. Defaults to repository default branch"},
				{Name: "*", In: "path", Required: true, Example: "src/main.go", Desc: "File path within the repository"},
			},
			ResponseExample: `package main

import "fmt"

func main() {
    fmt.Println("Hello, world!")
}`,
			Curl: `curl http://localhost:3000/api/v1/repos/my-repo/blob/master/src/main.go`,
			Notes: []string{
				"Content-Type is always text/plain; charset=utf-8 (even for binary files).",
				"Response is raw file content, not JSON.",
				"Path must point to a blob (file), not a tree (directory).",
				"Empty path returns 400.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/{name}/archive/{ref}",
			Summary:  "Download repository archive (ZIP)",
 			Params: []apiDocParam{
				{Name: "name", In: "path", Required: true, Example: "my-repo", Desc: "Repository name"},
				{Name: "ref", In: "path", Required: false, Example: "master", Desc: "Branch name, tag name, or commit OID. Defaults to repository default branch"},
			},
			Curl: `curl -OJ http://localhost:3000/api/v1/repos/my-repo/archive/master`,
			Notes: []string{
				"Content-Type: application/octet-stream.",
				"Content-Disposition: attachment; filename=<name>-<ref>.zip.",
				"ZIP contains all files recursively, prefixed with <repo-name>/.",
				"Submodules (gitlinks) are skipped.",
				"File paths in ZIP use the repo name as top-level directory.",
			},
		},
	},
}

func (h *HTTPHandler) serveAPIDocs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, apiDocs)
}
