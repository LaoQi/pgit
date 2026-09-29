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
	Title     string           `json:"title"`
	Endpoints []apiDocEndpoint `json:"endpoints"`
}

var apiDocs = apiDocData{
	Title: "pgit Management API",
	// 约定：仓库引用一律通过 ref 参数传递（仓库名或别名），不占路径段；
	// 因此 owner/repo 这类含斜杠的别名无需百分号编码。
	Endpoints: []apiDocEndpoint{
		{
			Method:  "GET",
			Path:    "/api/v1/repos",
			Summary: "List all repositories",
			ResponseExample: `{
  "total": 1,
  "repositories": [
    {
      "name": "my-repo",
      "description": "A demo repository",
      "aliases": ["my-repo"],
      "createdAt": "2026-06-24T10:00:00Z",
      "lastCommitTime": "2026-09-01T00:00:00Z"
    }
  ]
}`,
			Curl: "curl http://localhost:3000/api/v1/repos",
			Notes: []string{
				"Response is an object with total and repositories array, not a bare array.",
				"Repositories are sorted by lastCommitTime descending (newest commit first); repositories without commits come last, then by name.",
				"lastCommitTime is the max committer time of commits reachable from refs (annotated tags are peeled to their commit); the field is omitted for repositories without commits.",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/repos",
			Summary: "Create a new bare repository",
			Params: []apiDocParam{
				{Name: "name", In: "form", Required: true, Example: "my-repo", Desc: "New repository name (permanent identity, single segment; creates a new repo, not a ref lookup)"},
				{Name: "description", In: "form", Required: false, Example: "A demo repo", Desc: "Form field, NOT JSON body"},
				{Name: "defaultBranch", In: "form", Required: false, Example: "main", Desc: "Default branch name (initial HEAD), defaults to master"},
				{Name: "mirrorUrl", In: "form", Required: false, Example: "https://github.com/user/repo.git", Desc: "If present, creates a mirror repository that syncs from this remote URL (HTTP/HTTPS only)"},
				{Name: "mirrorInterval", In: "form", Required: false, Example: "300", Desc: "Sync interval in seconds (0=manual only, default 0). Only used when mirrorUrl is set"},
				{Name: "mirrorAuthType", In: "form", Required: false, Example: "basic", Desc: "Auth type: 'none' (default) or 'basic'. Only used when mirrorUrl is set"},
				{Name: "mirrorUsername", In: "form", Required: false, Example: "user", Desc: "Username for basic auth. Only used when mirrorAuthType=basic"},
				{Name: "mirrorPassword", In: "form", Required: false, Example: "token", Desc: "Password/token for basic auth. Only used when mirrorAuthType=basic"},
				{Name: "mirrorProxy", In: "form", Required: false, Example: "http://user:pass@127.0.0.1:7890", Desc: "HTTP proxy URL for mirror sync (http(s)://[user:pass@]host:port). Only used when mirrorUrl is set"},
			},
			RequestExample: `POST /api/v1/repos HTTP/1.1
Content-Type: application/x-www-form-urlencoded

name=my-repo&description=A%20demo%20repo&defaultBranch=main`,
			ResponseExample: `{
  "name": "my-repo",
  "description": "A demo repo",
  "aliases": ["my-repo"],
  "createdAt": "2026-06-24T10:00:00Z"
}`,
			Curl: `curl -X POST http://localhost:3000/api/v1/repos \
  -d "name=my-repo" \
  -d "description=A demo repo" \
  -d "defaultBranch=main"`,
			Notes: []string{
				"name is required and is the repository's permanent identity (single segment).",
				"description is a form field (application/x-www-form-urlencoded), NOT a JSON body.",
				"defaultBranch sets the initial HEAD symref target; defaults to 'master' if omitted.",
				"name is auto-added as an alias; the LAST alias in the list is the primary display ref (homepage shows & clone hint), so adding an alias makes it primary.",
				"Returns the full Repository object on success.",
				"Name validation (whitelist): chars A-Za-z0-9_-. only, alphanumeric/underscore ends, <=64 bytes, no slashes, no .git suffix, no reserved names (api/, webui prefix, healthz, metrics).",
				"When mirrorUrl form field is present, creates a mirror repository instead of a regular one.",
				"Mirror repos sync all refs from the remote (HTTP/HTTPS smart-http). First sync runs asynchronously.",
				"mirrorInterval=0 means manual sync only (POST /api/v1/repos/sync with ref).",
				"mirrorProxy sets an HTTP proxy for sync fetches (optional); URL userinfo enables proxy auth.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/info",
			Summary: "Get repository metadata and refs (branches/tags)",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
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
			Curl: "curl \"http://localhost:3000/api/v1/repos/info?ref=my-repo\"",
			Notes: []string{
				"Returns both metadata and refs in one response.",
				"refs includes branches (type=commit) and tags (type=tag).",
				"Empty repository returns refs as empty array [].",
				"This is the only way to get the list of branches/tags.",
			},
		},
		{
			Method:  "DELETE",
			Path:    "/api/v1/repos/info",
			Summary: "Delete a repository permanently",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "confirm", In: "query", Required: true, Example: "my-repo", Desc: "Must equal the repository name"},
			},
			RequestExample: `DELETE /api/v1/repos/info?ref=my-repo&confirm=my-repo HTTP/1.1`,
			Curl:           `curl -X DELETE "http://localhost:3000/api/v1/repos/info?ref=my-repo&confirm=my-repo"`,
			Notes: []string{
				"confirm must equal the repository's canonical name (even when ref is an alias).",
				"Returns empty body with 200 on success.",
				"Soft delete: a pgit.deleted marker is written inside the repository directory; all git data is kept on disk and the repository disappears from the API and git endpoints immediately.",
				"Startup scan skips marked directories. Delete the marker file and restart to restore the repository, or remove the directory to free the name.",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/repos/aliases",
			Summary: "Add a new alias to a repository",
			Params: []apiDocParam{
				{Name: "ref", In: "form", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "alias", In: "form", Required: true, Example: "group/repo", Desc: "Form field, can contain slashes"},
			},
			RequestExample: `POST /api/v1/repos/aliases HTTP/1.1
Content-Type: application/x-www-form-urlencoded

ref=my-repo&alias=group%2Frepo`,
			ResponseExample: `{
  "name": "my-repo",
  "description": "A demo repo",
  "aliases": ["my-repo", "group/repo"],
  "createdAt": "2026-06-24T10:00:00Z"
}`,
			Curl: `curl -X POST http://localhost:3000/api/v1/repos/aliases \
  -d "ref=my-repo" \
  -d "alias=group/repo"`,
			Notes: []string{
				"alias is a form field, NOT JSON body.",
				"Alias can contain slashes (e.g. group/repo) for nested paths.",
				"Alias is a form field; it may contain slashes (e.g. group/repo) and is matched literally.",
				"Alias rules: chars A-Za-z0-9_-. with / as separator, 1-8 segments, segment <=64 bytes, total <=100 bytes, no .git suffix, no reserved names (api/, webui prefix, healthz, metrics).",
				"Alias must not collide with any existing repository name or alias (case-insensitive); conflict returns 409.",
				"Returns the updated full Repository object.",
			},
		},
		{
			Method:  "DELETE",
			Path:    "/api/v1/repos/aliases",
			Summary: "Remove an alias from a repository",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "alias", In: "query", Required: true, Example: "group/repo", Desc: "Alias to remove (may contain slashes)"},
			},
			ResponseExample: `{
  "name": "my-repo",
  "description": "A demo repo",
  "aliases": ["my-repo"],
  "createdAt": "2026-06-24T10:00:00Z"
}`,
			Curl: `curl -X DELETE "http://localhost:3000/api/v1/repos/aliases?ref=my-repo&alias=group%2Frepo"`,
			Notes: []string{
				"The default alias (same as repo name) cannot be removed.",
				"alias is a form field, so aliases containing slashes can be removed without percent-encoding.",
				"Returns the updated full Repository object on success.",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/repos/default-branch",
			Summary: "Set repository default branch (HEAD symref)",
			Params: []apiDocParam{
				{Name: "ref", In: "form", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "branch", In: "form", Required: true, Example: "main", Desc: "Short branch name (e.g. 'main'), must already exist in refs/heads/"},
			},
			RequestExample: `POST /api/v1/repos/default-branch HTTP/1.1
Content-Type: application/x-www-form-urlencoded

ref=my-repo&branch=main`,
			ResponseExample: `{
   "ok": true,
   "defaultBranch": "main"
}`,
			Curl: `curl -X POST http://localhost:3000/api/v1/repos/default-branch \
  -d "ref=my-repo" \
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
			Path:    "/api/v1/repos/tree/{path...}",
			Summary: "Browse repository tree (directory listing)",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "treeish", In: "query", Required: false, Example: "master", Desc: "Branch name, tag name, or commit OID. Defaults to repository default branch"},
				{Name: "path", In: "path", Required: false, Example: "src/pkg", Desc: "Subdirectory path (trailing wildcard; empty means tree root)"},
			},
			ResponseExample: `[
   {"type": "tree", "hash": "4e6f77e...", "name": "src"},
   {"type": "blob", "hash": "a1b2c3d...", "name": "README.md"},
   {"type": "commit", "hash": "deadbef...", "name": "vendor/submodule"}
]`,
			Curl: `curl "http://localhost:3000/api/v1/repos/tree/src?ref=my-repo&treeish=master"`,
			Notes: []string{
				"Response is a bare JSON array, not wrapped in an object.",
				"ref supports: branch name, tag name, 'HEAD', or full 40-char commit OID.",
				"Default ref is repository's default branch (HEAD symref target), not hard-coded 'master'.",
				"type 'tree' = directory, 'blob' = file, 'commit' = gitlink/submodule.",
				"Empty repository returns 400 (ref not found).",
				"Use GET /api/v1/repos/info?ref=<name|alias> to list available refs.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/blob/{path...}",
			Summary: "Read file content (blob)",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "treeish", In: "query", Required: false, Example: "master", Desc: "Branch name, tag name, or commit OID. Defaults to repository default branch"},
				{Name: "path", In: "path", Required: true, Example: "src/main.go", Desc: "File path within the repository"},
			},
			ResponseExample: `package main

import "fmt"

func main() {
    fmt.Println("Hello, world!")
}`,
			Curl: `curl "http://localhost:3000/api/v1/repos/blob/src/main.go?ref=my-repo&treeish=master"`,
			Notes: []string{
				"Content-Type is always text/plain; charset=utf-8 (even for binary files).",
				"Response is raw file content, not JSON.",
				"Path must point to a blob (file), not a tree (directory).",
				"Empty path returns 400.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/archive",
			Summary: "Download repository archive (ZIP)",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "treeish", In: "query", Required: false, Example: "master", Desc: "Branch name, tag name, or commit OID. Defaults to repository default branch"},
			},
			Curl: `curl -OJ "http://localhost:3000/api/v1/repos/archive?ref=my-repo&treeish=master"`,
			Notes: []string{
				"Content-Type: application/octet-stream.",
				"Content-Disposition: attachment; filename=<name>-<treeish>.zip (slashes in treeish become dashes).",
				"ZIP contains all files recursively, prefixed with <repo-name>/.",
				"Submodules (gitlinks) are skipped.",
				"File paths in ZIP use the repo name as top-level directory.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/commits",
			Summary: "List recent commits on a ref",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-repo", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "treeish", In: "query", Required: false, Example: "master", Desc: "Branch name, tag name, or commit OID. Defaults to repository default branch"},
				{Name: "limit", In: "query", Required: false, Example: "10", Desc: "Max number of commits to return (default 20)"},
			},
			ResponseExample: `[
  {
    "hash": "a1b2c3d4e5f6...",
    "parents": ["f0e1d2c3b4a5..."],
    "author": "LaoQi",
    "email": "q@example.com",
    "timestamp": 1719216000,
    "subject": "initial commit"
  }
]`,
			Curl: `curl "http://localhost:3000/api/v1/repos/commits?ref=my-repo&treeish=master&limit=10"`,
			Notes: []string{
				"Response is a bare JSON array of commit objects.",
				"Walks the first-parent chain from the ref's commit.",
				"limit defaults to 20 if omitted or invalid.",
				"Empty repository or missing ref returns 400.",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/repos/sync",
			Summary: "Trigger manual sync for a mirror repository",
			Params: []apiDocParam{
				{Name: "ref", In: "form", Required: true, Example: "my-mirror", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
			},
			ResponseExample: `{
  "ok": true,
  "sync": {
    "timestamp": "2026-07-10T12:00:00Z",
    "duration": 1234,
    "success": true,
    "objectsFetched": 42,
    "refsUpdated": 3,
    "refsDeleted": 0,
    "upToDate": false,
    "trigger": "manual",
    "wants": 3,
    "haves": 1,
    "packSize": 12345
  }
}`,
			Curl: `curl -X POST http://localhost:3000/api/v1/repos/sync -d "ref=my-mirror"`,
			Notes: []string{
				"Only mirror repositories can be synced. Non-mirror repos return 400.",
				"Sync runs synchronously - response includes the sync result.",
				"If a sync is already in progress, returns 409 Conflict.",
				"trigger field is always 'manual' for this endpoint.",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/repos/settings",
			Summary: "Update repository description and mirror configuration",
			Params: []apiDocParam{
				{Name: "ref", In: "form", Required: true, Example: "my-mirror", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "description", In: "form", Required: true, Example: "Updated desc", Desc: "New description (empty string clears it)"},
				{Name: "mirrorRemoteUrl", In: "form", Required: false, Example: "https://github.com/user/repo.git", Desc: "New mirror remote URL (http/https). Mirror repos only"},
				{Name: "mirrorInterval", In: "form", Required: false, Example: "300", Desc: "New sync interval in seconds (0=manual). Mirror repos only. Changing it reschedules the sync timer"},
				{Name: "mirrorAuthType", In: "form", Required: false, Example: "basic", Desc: "Auth type: 'none' (default) or 'basic'. Mirror repos only"},
				{Name: "mirrorUsername", In: "form", Required: false, Example: "user", Desc: "Username for basic auth. Mirror repos only"},
				{Name: "mirrorPassword", In: "form", Required: false, Example: "token", Desc: "Password/token. Empty = keep current password. Mirror repos only"},
				{Name: "mirrorProxy", In: "form", Required: false, Example: "http://user:pass@127.0.0.1:7890", Desc: "HTTP proxy URL (empty=direct). Mirror repos only"},
			},
			RequestExample: `POST /api/v1/repos/settings HTTP/1.1
Content-Type: application/x-www-form-urlencoded

ref=my-mirror&description=Updated&mirrorRemoteUrl=https://github.com/user/repo.git&mirrorInterval=600&mirrorProxy=http://127.0.0.1:7890`,
			Curl: `curl -X POST http://localhost:3000/api/v1/repos/settings \
  -d "ref=my-mirror" \
  -d "description=Updated" \
  -d "mirrorInterval=600"`,
			Notes: []string{
				"description is always updated (empty clears it); applies to both regular and mirror repos.",
				"Mirror fields are only applied when the repo is a mirror; mirrorRemoteUrl/mirrorInterval/etc. are ignored for regular repos.",
				"mirrorPassword empty keeps the existing password (avoid re-entering on edits).",
				"Changing mirrorInterval reschedules the sync timer (0 stops scheduled sync, >0 starts/reschedules).",
				"Returns the updated Repository object on success.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/sync-log",
			Summary: "List sync log entries for a mirror repository",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-mirror", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
				{Name: "limit", In: "query", Required: false, Example: "20", Desc: "Max entries to return (default 50), newest first"},
			},
			ResponseExample: `{
  "entries": [
    {
      "timestamp": "2026-07-10T12:10:00Z",
      "duration": 0,
      "success": false,
      "error": "dial tcp: connection refused",
      "trigger": "scheduled",
      "queueWaitMs": 0,
      "wants": 0,
      "haves": 0,
      "packSize": 0
    },
    {
      "timestamp": "2026-07-10T12:05:00Z",
      "duration": 1234,
      "success": true,
      "objectsFetched": 42,
      "refsUpdated": 3,
      "refsDeleted": 0,
      "upToDate": false,
      "trigger": "scheduled",
      "wants": 3,
      "haves": 1,
      "packSize": 12345
    }
  ]
}`,
			Curl: `curl "http://localhost:3000/api/v1/repos/sync-log?ref=my-mirror&limit=20"`,
			Notes: []string{
				"Only mirror repositories have sync logs. Non-mirror repos return 400.",
				"Entries are returned newest-first.",
				"Never-synced mirror repos return empty array.",
				"Sync logs are stored in <repo>.git/pgit-sync.jsonl.",
				"trigger is one of initial (on register/startup), scheduled, manual. queueWaitMs is the time spent waiting in the task queue.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/repos/mirror-status",
			Summary: "Get mirror scheduling/sync status",
			Params: []apiDocParam{
				{Name: "ref", In: "query", Required: true, Example: "my-mirror", Desc: "Repository reference: canonical name or alias (alias may contain slashes)"},
			},
			ResponseExample: `{
  "repo": "my-mirror",
  "scheduled": true,
  "intervalSec": 600,
  "queued": false,
  "syncing": false,
  "lastSync": "2026-09-23T12:05:00Z",
  "lastError": "",
  "nextScheduled": "2026-09-23T12:15:00Z"
}`,
			Curl: `curl "http://localhost:3000/api/v1/repos/mirror-status?ref=my-mirror"`,
			Notes: []string{
				"scheduled=true means an active timer; intervalSec=0 means manual-only.",
				"queued=true means the sync task is waiting in the task queue; syncing=true means it is executing now.",
				"Sync tasks run in a bounded queue (mirrorMaxConcurrentSyncs, default 5); scheduled tasks are dropped when the queue is full.",
				"lastError is the most recent failure message (empty on success).",
				"nextScheduled is approximate (interval from now).",
				"Non-mirror repos return 400; unknown repos return 404.",
			},
		},
		{
			Method:  "GET",
			Path:    "/api/v1/github/repos",
			Summary: "Discover repositories of a GitHub account (read-only, no local changes)",
			Params: []apiDocParam{
				{Name: "owner", In: "query", Required: true, Example: "LaoQi", Desc: "GitHub user or organization name"},
				{Name: "token", In: "query", Required: false, Example: "ghp_xxx", Desc: "Personal access token. Without it only public repositories are returned"},
				{Name: "apiBase", In: "query", Required: false, Example: "https://api.github.com", Desc: "API base URL (default https://api.github.com, set it for GitHub Enterprise)"},
				{Name: "proxy", In: "query", Required: false, Example: "http://127.0.0.1:7890", Desc: "HTTP proxy for the API calls (empty=direct)"},
				{Name: "includeForks", In: "query", Required: false, Example: "false", Desc: "Include forked repositories (default false)"},
				{Name: "includeArchived", In: "query", Required: false, Example: "true", Desc: "Include archived repositories (default true)"},
				{Name: "namePrefix", In: "query", Required: false, Example: "gh-", Desc: "Prefix for the suggested local repository name (default empty)"},
			},
			RequestExample: `GET /api/v1/github/repos?owner=LaoQi&includeForks=false HTTP/1.1
X-Github-Token: ghp_xxx`,
			ResponseExample: `{
  "owner": "LaoQi",
  "total": 2,
  "creatable": 1,
  "repositories": [
    {
      "fullName": "LaoQi/alpha",
      "name": "alpha",
      "owner": "LaoQi",
      "private": false,
      "fork": false,
      "archived": false,
      "description": "alpha desc",
      "defaultBranch": "main",
      "sizeKb": 42,
      "updatedAt": "2026-09-01T00:00:00Z",
      "cloneUrl": "https://github.com/LaoQi/alpha.git",
      "localName": "LaoQi_alpha",
      "conflict": ""
    },
    {
      "fullName": "LaoQi/beta",
      "name": "beta",
      "owner": "LaoQi",
      "localName": "LaoQi_beta",
      "conflict": "mirror-same-remote",
      "cloneUrl": "https://github.com/LaoQi/beta.git"
    }
  ]
}`,
			Curl: `curl "http://localhost:3000/api/v1/github/repos?owner=LaoQi" -H "X-Github-Token: ghp_xxx"`,
			Notes: []string{
				"Read-only: nothing is created or modified on the server.",
				"Without token only public repositories are listed; private ones require a token with repo scope (the token is never logged).",
				"Pass the token via the X-Github-Token header to keep it out of URLs and logs.",
				"localName is the suggested local repository name: {namePrefix}{owner}_{repo}.",
				"conflict: empty=importable, not-a-mirror=local repo with that name exists, mirror-same-remote=already mirrored from the same URL, mirror-other-remote=mirrored from another URL, alias-exists=the {owner}/{repo} alias is taken.",
				"GitHub rate limits apply (60/h unauthenticated, 5000/h with token); 429 is returned when exceeded, 404 when the owner does not exist.",
			},
		},
		{
			Method:  "POST",
			Path:    "/api/v1/github/import",
			Summary: "Create mirror repositories for the selected GitHub repositories",
			Params: []apiDocParam{
				{Name: "owner", In: "form", Required: true, Example: "LaoQi", Desc: "GitHub user or organization name"},
				{Name: "repos", In: "form", Required: true, Example: "alpha", Desc: "Repository name, repeat the field for multiple repos (owner/repo form also accepted)"},
				{Name: "token", In: "form", Required: false, Example: "ghp_xxx", Desc: "Token lists & imports private repos; persisted ONLY in private repos as basic auth (public repos sync anonymously, token not stored). Empty = public repos only"},
				{Name: "apiBase", In: "form", Required: false, Example: "https://api.github.com", Desc: "API base URL (default https://api.github.com)"},
				{Name: "cloneBase", In: "form", Required: false, Example: "https://github.com", Desc: "Override the clone base URL (default: the clone_url returned by GitHub)"},
				{Name: "proxy", In: "form", Required: false, Example: "http://127.0.0.1:7890", Desc: "HTTP proxy for API calls and mirror sync (empty=direct)"},
				{Name: "namePrefix", In: "form", Required: false, Example: "gh-", Desc: "Prefix for the local repository name (default empty)"},
				{Name: "syncInterval", In: "form", Required: false, Example: "3600", Desc: "Sync interval in seconds for the created mirrors (0=manual only, default 0)"},
			},
			RequestExample: `POST /api/v1/github/import HTTP/1.1
Content-Type: application/x-www-form-urlencoded

owner=LaoQi&repos=alpha&repos=beta&syncInterval=3600`,
			ResponseExample: `{
  "ok": false,
  "created": 1,
  "failed": 1,
  "results": [
    {
      "repo": "alpha",
      "localName": "LaoQi_alpha",
      "remoteUrl": "https://github.com/LaoQi/alpha.git",
      "aliases": ["LaoQi_alpha", "LaoQi/alpha"],
      "description": "alpha desc",
      "ok": true
    },
    {
      "repo": "beta",
      "ok": false,
      "error": "already mirrored from https://github.com/LaoQi/beta.git"
    }
  ]
}`,
			Curl: `curl -X POST http://localhost:3000/api/v1/github/import   -H "X-Github-Token: ghp_xxx"   -d "owner=LaoQi" -d "repos=alpha" -d "repos=beta" -d "syncInterval=3600"`,
			Notes: []string{
				"Creates one mirror repository per selected repo: name {namePrefix}{owner}_{repo}, alias {owner}/{repo} (clone URL /{owner}/{repo}.git).",
				"Each created mirror keeps the GitHub description and default branch, and registers its sync timer (initial sync is enqueued immediately).",
				"Per-repo failures do not abort the batch; check results[] for details. Existing repositories are never modified or deleted.",
				"Empty repos[] returns 400; more than 200 repos returns 400.",
				"Token is stored in each repository's pgit.json as basic auth password (same as a manually created mirror).",
			},
		},
	},
}

func (h *HTTPHandler) serveAPIDocs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, apiDocs)
}
