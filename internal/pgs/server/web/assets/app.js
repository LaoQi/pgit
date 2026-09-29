'use strict';

var API = '/api/v1';
var BASE = (function() {
    try { return new URL(document.baseURI).pathname.replace(/\/+$/, ''); }
    catch(e) { return ''; }
})();

var toastTimer = null;

function esc(s) {
    var d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
}

function escAttr(s) {
    return String(s == null ? '' : s)
        .replace(/&/g, '&amp;')
        .replace(/"/g, '&quot;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;');
}

function enc(s) { return encodeURIComponent(s); }

// 仓库引用一律作为 ref 参数（仓库名或别名），不占路径段：多段别名无需编码。
function repoUrl(ref) { return '/repo/info?ref=' + enc(ref); }

function treePageUrl(ref, treeish, path) {
    var url = '/repo/tree';
    if (path) url += '/' + path.split('/').map(enc).join('/');
    return url + '?ref=' + enc(ref) + '&treeish=' + enc(treeish);
}

function fmtDate(iso) {
    try { return new Date(iso).toLocaleString(); }
    catch(e) { return iso; }
}

function fmtTs(ts) {
    try { return new Date(ts * 1000).toLocaleString(); }
    catch(e) { return String(ts); }
}

function fmtBytes(n) {
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(1) + ' GB';
}

function showToast(msg, type) {
    var el = document.getElementById('toast');
    el.textContent = msg;
    el.className = 'toast show' + (type ? ' ' + type : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function() { el.className = 'toast'; }, 3000);
}

function copyText(text) {
    navigator.clipboard.writeText(text).then(
        function() { showToast('Copied'); },
        function() { showToast('Copy failed', 'error'); }
    );
}

function apiJSON(path) {
    return fetch(path).then(function(res) {
        if (!res.ok) {
            return res.json().catch(function() { return { error: res.statusText }; })
                .then(function(err) { throw new Error(err.error || res.statusText); });
        }
        return res.json();
    });
}

function apiText(path) {
    return fetch(path).then(function(res) {
        if (!res.ok) {
            return res.json().catch(function() { return { error: res.statusText }; })
                .then(function(err) { throw new Error(err.error || res.statusText); });
        }
        return res.text();
    });
}

function apiForm(method, path, params) {
    var body = new URLSearchParams();
    if (params) {
        Object.keys(params).forEach(function(k) { body.set(k, params[k]); });
    }
    return fetch(path, {
        method: method,
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: body
    }).then(function(res) {
        return res.json().catch(function() { return { error: res.statusText }; }).then(function(data) {
            if (!res.ok) throw new Error(data.error || res.statusText);
            return data;
        });
    });
}

function apiDelete(path) {
    return fetch(path, { method: 'DELETE' }).then(function(res) {
        if (!res.ok) {
            return res.json().catch(function() { return { error: res.statusText }; })
                .then(function(err) { throw new Error(err.error || res.statusText); });
        }
        return res;
    });
}

function navigate(path) {
    history.pushState(null, '', BASE + path);
    route();
}

function currentPath() {
    var p = window.location.pathname.replace(/\/+$/, '');
    if (p === BASE) return '/';
    if (p.indexOf(BASE + '/') === 0) return p.slice(BASE.length);
    return '/';
}

function route() {
    var path = currentPath();
    var app = document.getElementById('app');
    app.innerHTML = '<div class="loading">Loading...</div>';
    var query = new URLSearchParams(window.location.search);
    if (path === '/') {
        viewRepos(app);
    } else if (path === '/repo/info') {
        viewRepoDetail(app, query.get('ref') || '');
    } else if (path === '/repo/tree' || path.indexOf('/repo/tree/') === 0) {
        var subPath = path === '/repo/tree' ? '' : path.slice('/repo/tree/'.length);
        viewTree(app, query.get('ref') || '', query.get('treeish') || '',
            subPath.split('/').map(decodeURIComponent).join('/'));
    } else if (path === '/import') {
        viewImport(app);
    } else if (path === '/api') {
        viewApiDocs(app);
    } else {
        app.innerHTML = '<div class="error">404 - Page not found. <a href="." data-link="/">Go home</a></div>';
    }
}

window.addEventListener('popstate', route);

document.addEventListener('click', function(e) {
    var el = e.target.closest('[data-link]');
    if (el) { e.preventDefault(); navigate(el.getAttribute('data-link')); return; }
    el = e.target.closest('[data-copy]');
    if (el) { e.preventDefault(); copyText(el.getAttribute('data-copy')); return; }
    el = e.target.closest('[data-blob-url]');
    if (el) { viewBlob(el.getAttribute('data-blob-url'), el.getAttribute('data-blob-name')); return; }
    el = e.target.closest('[data-remove-alias]');
    if (el) {
        removeAlias(el.getAttribute('data-remove-repo'), el.getAttribute('data-remove-alias'));
        return;
    }
});

document.addEventListener('DOMContentLoaded', route);

function viewRepos(app) {
    apiJSON(API + '/repos').then(function(data) {
        var repos = data.repositories || [];
        repos.sort(function(a, b) { return (b.createdAt || '').localeCompare(a.createdAt || ''); });
        var html = '<div class="flex-between mb-16"><h2>Repositories</h2>'
            + '<button class="btn btn-primary btn-sm" id="toggleNewBtn">New Repository</button></div>'
            + '<div id="newRepoForm" style="display:none">'
            + '<div class="toggle-form">'
            + '<div class="form-group"><label>Name</label><input id="repoName" placeholder="my-repo" autocomplete="off"></div>'
            + '<div class="form-group"><label>Description</label><input id="repoDesc" placeholder="optional" autocomplete="off"></div>'
            + '<div class="form-group"><label class="checkbox-label"><input type="checkbox" id="repoIsMirror"> Mirror Repository</label></div>'
            + '<div id="normalFields">'
            + '<div class="form-group"><label>Default Branch</label><input id="repoDefaultBranch" value="master" placeholder="master" autocomplete="off"></div>'
            + '</div>'
            + '<div id="mirrorFields" style="display:none">'
            + '<div class="form-group"><label>Remote URL</label><input id="mirrorUrl" placeholder="https://github.com/user/repo.git" autocomplete="off"></div>'
            + '<div class="form-group"><label>Sync Interval (seconds, 0=manual)</label><input id="mirrorInterval" value="0" placeholder="0" autocomplete="off"></div>'
            + '<div class="form-group"><label>Auth Type</label><select id="mirrorAuthType"><option value="none">None</option><option value="basic">Basic Auth</option></select></div>'
            + '<div id="mirrorAuthFields" style="display:none">'
            + '<div class="form-group"><label>Username</label><input id="mirrorUsername" placeholder="username" autocomplete="off"></div>'
            + '<div class="form-group"><label>Password / Token</label><input id="mirrorPassword" type="password" placeholder="password or token" autocomplete="off"></div>'
            + '</div>'
            + '<div class="form-group"><label>Proxy (optional)</label><input id="mirrorProxy" placeholder="http://user:pass@host:port" autocomplete="off"></div>'
            + '</div>'
            + '<button class="btn btn-primary btn-sm" id="createRepoBtn">Create</button>'
            + '</div></div>';

        if (repos.length > 0) {
            html += '<input class="search-input" id="repoSearch" placeholder="Search repositories..." autocomplete="off">';
        }

        if (repos.length === 0) {
            html += '<div class="empty">No repositories yet. Create one to get started.</div>';
        } else {
            html += '<div class="repo-grid">';
            repos.forEach(function(r) {
                var aliases = r.aliases || [];
                // 首选展示 ref = 末位别名：GitHub 导入的 {owner}/{repo} 由 AddAlias 追加在末位，
                // 新增别名即成为新首选；无别名时回落仓库名。
                var primaryRef = aliases[aliases.length - 1] || r.name;
                var host = window.location.host;
                var httpClone = window.location.protocol + '//' + host + '/' + primaryRef + '.git';
                var sshClone = 'ssh://' + host + '/' + primaryRef + '.git';
                var link = repoUrl(primaryRef);
                var mirrorBadge = r.mirror ? ' <span class="badge badge-mirror">mirror</span>' : '';
                var haystack = (r.name + ' ' + (r.description || '') + ' ' + aliases.join(' ')).toLowerCase();
                html += '<div class="repo-card" data-search="' + escAttr(haystack) + '">'
                    + '<div class="name"><a href="' + escAttr(link) + '" data-link="' + escAttr(link) + '">' + esc(primaryRef) + '</a>' + mirrorBadge + '</div>'
                    + '<div class="desc">' + esc(r.description || 'No description') + '</div>'
                    + '<div class="meta"><span>aliases: ' + aliases.length + '</span><span>' + esc(fmtDate(r.createdAt)) + '</span></div>'
                    + '<div class="clone-box"><span class="label">HTTP</span><span class="url">' + esc(httpClone) + '</span>'
                    + '<button class="copy-btn" data-copy="' + escAttr(httpClone) + '">copy</button></div>'
                    + '<div class="clone-box"><span class="label">SSH</span><span class="url">' + esc(sshClone) + '</span>'
                    + '<button class="copy-btn" data-copy="' + escAttr(sshClone) + '">copy</button></div>'
                    + '</div>';
            });
            html += '</div>';
            html += '<div class="empty" id="repoNoMatch" style="display:none">No repositories match your search.</div>';
        }
        app.innerHTML = html;

        var repoSearch = document.getElementById('repoSearch');
        if (repoSearch) {
            var repoCards = [].slice.call(app.querySelectorAll('.repo-card'));
            var repoNoMatch = document.getElementById('repoNoMatch');
            repoSearch.addEventListener('input', function() {
                var q = this.value.trim().toLowerCase();
                var shown = 0;
                repoCards.forEach(function(card) {
                    var hit = !q || (card.getAttribute('data-search') || '').indexOf(q) >= 0;
                    card.style.display = hit ? '' : 'none';
                    if (hit) shown++;
                });
                if (repoNoMatch) repoNoMatch.style.display = shown === 0 ? 'block' : 'none';
            });
        }

        document.getElementById('toggleNewBtn').addEventListener('click', function() {
            var form = document.getElementById('newRepoForm');
            form.style.display = form.style.display === 'none' ? 'block' : 'none';
        });
        document.getElementById('repoIsMirror').addEventListener('change', function() {
            var isMirror = this.checked;
            document.getElementById('normalFields').style.display = isMirror ? 'none' : 'block';
            document.getElementById('mirrorFields').style.display = isMirror ? 'block' : 'none';
        });
        document.getElementById('mirrorAuthType').addEventListener('change', function() {
            document.getElementById('mirrorAuthFields').style.display = this.value === 'basic' ? 'block' : 'none';
        });
        document.getElementById('createRepoBtn').addEventListener('click', function() {
            var name = document.getElementById('repoName').value.trim();
            var desc = document.getElementById('repoDesc').value.trim();
            if (!name) { showToast('Name is required', 'error'); return; }
            var isMirror = document.getElementById('repoIsMirror').checked;
            if (isMirror) {
                var mirrorUrl = document.getElementById('mirrorUrl').value.trim();
                if (!mirrorUrl) { showToast('Remote URL is required', 'error'); return; }
                var params = { name: name, description: desc, mirrorUrl: mirrorUrl };
                var interval = document.getElementById('mirrorInterval').value.trim();
                if (interval) params.mirrorInterval = interval;
                var authType = document.getElementById('mirrorAuthType').value;
                params.mirrorAuthType = authType;
                if (authType === 'basic') {
                    params.mirrorUsername = document.getElementById('mirrorUsername').value.trim();
                    params.mirrorPassword = document.getElementById('mirrorPassword').value;
                }
                var proxy = document.getElementById('mirrorProxy').value.trim();
                if (proxy) params.mirrorProxy = proxy;
                apiForm('POST', API + '/repos', params).then(function() {
                    showToast('Mirror repository created, sync will start shortly');
                    navigate(repoUrl(name));
                }).catch(function(err) { showToast(err.message, 'error'); });
            } else {
                var defaultBranch = document.getElementById('repoDefaultBranch').value.trim() || 'master';
                apiForm('POST', API + '/repos', { name: name, description: desc, defaultBranch: defaultBranch }).then(function() {
                    showToast('Repository created');
                    navigate(repoUrl(name));
                }).catch(function(err) { showToast(err.message, 'error'); });
            }
        });
    }).catch(function(err) {
        app.innerHTML = '<div class="error">' + esc(err.message) + '</div>';
    });
}

function viewRepoDetail(app, ref) {
    if (!ref) {
        app.innerHTML = '<div class="error">Missing repository reference.</div>';
        return;
    }
    apiJSON(API + '/repos/info?ref=' + enc(ref)).then(function(data) {
        var repo = data.metadata || {};
        var refs = data.refs || [];
        var aliases = repo.aliases || [];
        var host = window.location.host;

        var html = '<div class="breadcrumb"><a href="." data-link="/">Repositories</a>'
            + '<span class="sep">/</span><strong>' + esc(repo.name) + '</strong></div>';

         html += '<div class="card"><h2>' + esc(repo.name) + '</h2>'
             + '<p class="text-muted">' + esc(repo.description || 'No description') + '</p>'
             + '<p class="text-sm text-muted mt-8">Created: ' + esc(fmtDate(repo.createdAt)) + '</p>'
             + '<p class="text-sm text-muted mt-4">Default Branch: <strong>' + esc(data.defaultBranch || 'master') + '</strong></p></div>';

         if (repo.mirror) {
             var m = repo.mirror;
             var intervalText = m.syncInterval > 0 ? 'Every ' + m.syncInterval + 's' : 'Manual only';
             var lastSyncText = m.lastSync ? fmtDate(m.lastSync) : 'Never';
             var errorHtml = m.lastError ? '<p class="text-sm mt-4" style="color:var(--red)">Last Error: ' + esc(m.lastError) + '</p>' : '';
             var authText = m.authType === 'basic' ? 'Basic Auth (' + esc(m.username || '') + ')' : 'None';
             var proxyText = m.proxy ? esc(m.proxy) : 'Direct (no proxy)';
             html += '<div class="card"><h3>Mirror</h3>'
                 + '<p class="text-sm text-muted">Remote: <code>' + esc(m.remoteUrl) + '</code></p>'
                 + '<p class="text-sm text-muted mt-4">Sync: ' + esc(intervalText) + '</p>'
                 + '<p class="text-sm text-muted mt-4">Auth: ' + esc(authText) + '</p>'
                 + '<p class="text-sm text-muted mt-4">Proxy: ' + proxyText + '</p>'
                 + '<p class="text-sm text-muted mt-4">Last Sync: ' + esc(lastSyncText) + '</p>'
                 + errorHtml
                 + '<button class="btn btn-primary btn-sm mt-8" id="syncNowBtn">Sync Now</button></div>';
         }

         html += '<div class="card"><h3>Clone URLs</h3>';
         aliases.forEach(function(a) {
             var httpUrl = window.location.protocol + '//' + host + '/' + a + '.git';
             var sshUrl = 'ssh://' + host + '/' + a + '.git';
             html += '<div class="clone-box"><span class="label">HTTP</span><span class="url">' + esc(httpUrl) + '</span>'
                 + '<button class="copy-btn" data-copy="' + escAttr(httpUrl) + '">copy</button></div>';
             html += '<div class="clone-box"><span class="label">SSH</span><span class="url">' + esc(sshUrl) + '</span>'
                 + '<button class="copy-btn" data-copy="' + escAttr(sshUrl) + '">copy</button></div>';
         });
         html += '</div>';

         html += '<div class="card"><h3>Branches &amp; Tags</h3>';
         if (refs.length === 0) {
             html += '<div class="empty">This Repository is empty. Push some commits to get started.</div>';
         } else {
             html += '<table><thead><tr><th>Type</th><th>Name</th><th>Subject</th><th>Author</th><th>Date</th><th></th></tr></thead><tbody>';
             refs.forEach(function(ref) {
                 var badge = ref.type === 'tag' ? '<span class="badge badge-tag">tag</span>' : '<span class="badge badge-commit">branch</span>';
                 var treeLink = treePageUrl(repo.name, ref.name, '');
                 html += '<tr><td>' + badge + '</td><td>' + esc(ref.name) + '</td>'
                     + '<td>' + esc(ref.subject || '') + '</td>'
                     + '<td>' + esc(ref.author || '') + '</td>'
                     + '<td class="text-muted">' + esc(fmtTs(ref.timestamp)) + '</td>'
                     + '<td><a href="' + escAttr(treeLink) + '" data-link="' + escAttr(treeLink) + '" class="btn btn-sm">Browse</a></td>'
                     + '</tr>';
             });
             html += '</tbody></table>';
         }
         html += '</div>';

         if (refs.length > 0) {
             var defaultRef = data.defaultBranch || 'master';
             html += '<div class="card"><h3>Recent Commits</h3>'
                 + '<div id="commitsList" class="commits-list"><div class="loading">Loading commits...</div></div></div>';
         }

         if (refs.length > 0) {
             html += '<div class="card"><h3>Download Archive</h3>'
                 + '<div class="flex"><select id="archiveRef">';
             refs.forEach(function(ref) {
                 html += '<option value="' + escAttr(ref.name) + '">' + esc(ref.name) + ' (' + ref.type + ')</option>';
             });
             html += '</select><button class="btn btn-sm" id="downloadArchiveBtn">Download ZIP</button></div></div>';
         }

         html += '<div class="card"><h3>Aliases</h3>';
         aliases.forEach(function(a) {
             html += '<div class="alias-item"><span class="alias-name">' + esc(a) + '</span>';
             if (a === repo.name) {
                 html += '<span class="text-muted text-sm">(default)</span>';
             } else {
                 html += '<a class="btn btn-sm" href="' + escAttr(repoUrl(a)) + '" data-link="' + escAttr(repoUrl(a)) + '">open</a> '
                     + '<button class="btn btn-danger btn-sm" data-remove-repo="' + escAttr(repo.name) + '" data-remove-alias="' + escAttr(a) + '">Remove</button>';
             }
             html += '</div>';
         });
         html += '<div class="form-group mt-16"><label>Add Alias</label>'
             + '<div class="flex"><input id="newAlias" placeholder="group/repo" autocomplete="off">'
             + '<button class="btn btn-sm" id="addAliasBtn">Add</button></div></div>';
         html += '</div>';

         // Add set default branch card if there are branches
         var branches = refs.filter(function(r) { return r.type === 'commit'; });
         if (branches.length > 0) {
             html += '<div class="card"><h3>Default Branch</h3>'
                 + '<p class="text-muted mt-8">Current: <strong>' + esc(data.defaultBranch || 'master') + '</strong></p>'
                 + '<div class="form-group mt-8"><label>Change to:</label>'
                 + '<div class="flex"><select id="newDefaultBranch">';
             branches.forEach(function(b) {
                 var selected = b.name === data.defaultBranch ? ' selected' : '';
                 html += '<option value="' + escAttr(b.name) + '"' + selected + '>' + esc(b.name) + '</option>';
             });
             html += '</select><button class="btn btn-sm" id="setDefaultBranchBtn">Set</button></div></div>'
                 + '</div>';
         }

         if (repo.mirror) {
             html += '<div class="card"><h3>Sync Log</h3>'
                 + '<div id="syncLogList" class="sync-log-list"><div class="loading">Loading sync log...</div></div></div>';
         }

         html += '<div class="card"><h3>Settings</h3>'
             + '<div class="form-group"><label>Description</label><input id="setDesc" value="' + escAttr(repo.description || '') + '"></div>';
         if (repo.mirror) {
             var sm = repo.mirror;
             html += '<div class="form-group"><label>Remote URL</label><input id="setMirrorUrl" value="' + escAttr(sm.remoteUrl || '') + '"></div>'
                 + '<div class="form-group"><label>Sync Interval (seconds, 0=manual)</label><input id="setMirrorInterval" value="' + escAttr(String(sm.syncInterval || 0)) + '"></div>'
                 + '<div class="form-group"><label>Auth Type</label><select id="setMirrorAuthType"><option value="none"' + (sm.authType !== 'basic' ? ' selected' : '') + '>None</option><option value="basic"' + (sm.authType === 'basic' ? ' selected' : '') + '>Basic Auth</option></select></div>'
                 + '<div id="setMirrorAuthFields" style="' + (sm.authType === 'basic' ? 'display:block' : 'display:none') + '">'
                 + '<div class="form-group"><label>Username</label><input id="setMirrorUsername" value="' + escAttr(sm.username || '') + '"></div>'
                 + '<div class="form-group"><label>Password / Token</label><input id="setMirrorPassword" type="password" placeholder="leave blank to keep current" autocomplete="off"></div>'
                 + '</div>'
                 + '<div class="form-group"><label>Proxy (optional)</label><input id="setMirrorProxy" value="' + escAttr(sm.proxy || '') + '" placeholder="http://user:pass@host:port"></div>';
         }
         html += '<button class="btn btn-primary btn-sm mt-8" id="saveSettingsBtn">Save Settings</button></div>';

         html += '<div class="card"><h3>Danger Zone</h3>'
             + '<p class="text-muted text-sm">Delete this repository. This action cannot be undone.</p>'
             + '<button class="btn btn-danger btn-sm" id="deleteRepoBtn">Delete Repository</button></div>';

         app.innerHTML = html;

        if (document.getElementById('downloadArchiveBtn')) {
            document.getElementById('downloadArchiveBtn').addEventListener('click', function() {
                var ref = document.getElementById('archiveRef').value;
                window.open(API + '/repos/archive?ref=' + enc(repo.name) + '&treeish=' + enc(ref), '_blank');
            });
        }
        document.getElementById('addAliasBtn').addEventListener('click', function() {
            var alias = document.getElementById('newAlias').value.trim();
            if (!alias) { showToast('Alias is required', 'error'); return; }
            apiForm('POST', API + '/repos/aliases', { ref: repo.name, alias: alias }).then(function() {
                showToast('Alias added');
                viewRepoDetail(app, ref);
            }).catch(function(err) { showToast(err.message, 'error'); });
        });
         document.getElementById('deleteRepoBtn').addEventListener('click', function() {
             var input = prompt('Type the repository name to confirm deletion:', '');
             if (input !== repo.name) { showToast('Confirmation mismatch', 'error'); return; }
             apiDelete(API + '/repos/info?ref=' + enc(repo.name) + '&confirm=' + enc(repo.name)).then(function() {
                 showToast('Repository deleted');
                 navigate('/');
             }).catch(function(err) { showToast(err.message, 'error'); });
         });
          if (document.getElementById('setDefaultBranchBtn')) {
              document.getElementById('setDefaultBranchBtn').addEventListener('click', function() {
                  var branch = document.getElementById('newDefaultBranch').value;
                  if (!branch) { showToast('Select a branch', 'error'); return; }
                  apiForm('POST', API + '/repos/default-branch', { ref: repo.name, branch: branch }).then(function() {
                      showToast('Default branch updated to ' + branch);
                      viewRepoDetail(app, ref);
                  }).catch(function(err) { showToast(err.message, 'error'); });
              });
          }

          var saveSettingsBtn = document.getElementById('saveSettingsBtn');
          if (saveSettingsBtn) {
              saveSettingsBtn.addEventListener('click', function() {
                  var params = { ref: repo.name, description: document.getElementById('setDesc').value };
                  if (repo.mirror) {
                      params.mirrorRemoteUrl = document.getElementById('setMirrorUrl').value.trim();
                      params.mirrorInterval = document.getElementById('setMirrorInterval').value.trim();
                      params.mirrorAuthType = document.getElementById('setMirrorAuthType').value;
                      params.mirrorProxy = document.getElementById('setMirrorProxy').value.trim();
                      if (params.mirrorAuthType === 'basic') {
                          params.mirrorUsername = document.getElementById('setMirrorUsername').value.trim();
                          var pw = document.getElementById('setMirrorPassword').value;
                          if (pw) params.mirrorPassword = pw;
                      }
                  }
                  apiForm('POST', API + '/repos/settings', params).then(function() {
                      showToast('Settings saved');
                      viewRepoDetail(app, ref);
                  }).catch(function(err) { showToast(err.message, 'error'); });
              });
          }
          var setMirrorAuthSelect = document.getElementById('setMirrorAuthType');
          if (setMirrorAuthSelect) {
              setMirrorAuthSelect.addEventListener('change', function() {
                  document.getElementById('setMirrorAuthFields').style.display = this.value === 'basic' ? 'block' : 'none';
              });
          }

          var commitsContainer = document.getElementById('commitsList');
         if (commitsContainer) {
             var commitsRef = data.defaultBranch || 'master';
             apiJSON(API + '/repos/commits?ref=' + enc(repo.name) + '&treeish=' + enc(commitsRef) + '&limit=20').then(function(commits) {
                 if (!commits || commits.length === 0) {
                     commitsContainer.innerHTML = '<div class="empty">No commits found.</div>';
                     return;
                 }
                 var html = '';
                 commits.forEach(function(c) {
                     var shortHash = c.hash.slice(0, 8);
                     html += '<div class="commit-entry">'
                         + '<div class="commit-hash" data-copy="' + escAttr(c.hash) + '" title="Click to copy full hash">' + esc(shortHash) + '</div>'
                         + '<div class="commit-subject">' + esc(c.subject) + '</div>'
                         + '<div class="commit-meta">' + esc(c.author) + ' &middot; ' + esc(fmtTs(c.timestamp)) + '</div>'
                         + '</div>';
                 });
                 commitsContainer.innerHTML = html;
             }).catch(function() {
                 commitsContainer.innerHTML = '<div class="empty">Failed to load commits.</div>';
             });
         }

         var syncNowBtn = document.getElementById('syncNowBtn');
         if (syncNowBtn) {
             syncNowBtn.addEventListener('click', function() {
                 syncNowBtn.disabled = true;
                 syncNowBtn.textContent = 'Syncing...';
                 apiForm('POST', API + '/repos/sync', { ref: repo.name }).then(function(data) {
                     var sync = data.sync || {};
                     if (sync.success) {
                         showToast('Sync completed: ' + sync.objectsFetched + ' objects, ' + sync.refsUpdated + ' refs updated');
                     } else {
                         showToast('Sync failed: ' + (sync.error || 'unknown error'), 'error');
                     }
                     viewRepoDetail(app, ref);
                 }).catch(function(err) {
                     showToast(err.message, 'error');
                     syncNowBtn.disabled = false;
                     syncNowBtn.textContent = 'Sync Now';
                 });
             });
         }

         var syncLogContainer = document.getElementById('syncLogList');
         if (syncLogContainer) {
             apiJSON(API + '/repos/sync-log?ref=' + enc(repo.name) + '&limit=20').then(function(data) {
                 var entries = data.entries || [];
                 if (entries.length === 0) {
                     syncLogContainer.innerHTML = '<div class="empty">No sync history yet.</div>';
                     return;
                 }
                 var html = '';
                entries.forEach(function(e) {
                    var statusBadge = e.success
                        ? '<span class="badge badge-sync-ok">OK</span>'
                        : '<span class="badge badge-sync-fail">FAIL</span>';
                    var triggerBadge = '<span class="badge badge-sync-trigger">' + esc(e.trigger) + '</span>';
                    var detailParts = [];
                    if (e.wants > 0) detailParts.push('wants=' + e.wants);
                    if (e.haves > 0) detailParts.push('haves=' + e.haves);
                    if (e.upToDate) detailParts.push('up-to-date');
                    if (e.objectsFetched > 0) detailParts.push(e.objectsFetched + ' objects');
                    if (e.refsUpdated > 0) detailParts.push(e.refsUpdated + ' refs updated');
                    if (e.refsDeleted > 0) detailParts.push(e.refsDeleted + ' refs deleted');
                    if (e.packSize > 0) detailParts.push(fmtBytes(e.packSize));
                    if (e.duration > 0) detailParts.push(e.duration + 'ms');
                    var detail = detailParts.length > 0 ? esc(detailParts.join(', ')) : '';
                    var errorHtml = e.error ? '<div class="sync-log-error">' + esc(e.error) + '</div>' : '';
                    html += '<div class="sync-log-entry">'
                        + '<div class="sync-log-header">' + statusBadge + triggerBadge
                        + '<span class="sync-log-time">' + esc(fmtDate(e.timestamp)) + '</span></div>'
                        + (detail ? '<div class="sync-log-detail">' + detail + '</div>' : '')
                        + errorHtml
                         + '</div>';
                 });
                 syncLogContainer.innerHTML = html;
             }).catch(function() {
                 syncLogContainer.innerHTML = '<div class="empty">Failed to load sync log.</div>';
             });
         }
     }).catch(function(err) {
        app.innerHTML = '<div class="error">' + esc(err.message) + '</div>';
    });
}

function removeAlias(repoName, alias) {
    apiDelete(API + '/repos/aliases?ref=' + enc(repoName) + '&alias=' + enc(alias)).then(function() {
        showToast('Alias removed');
        viewRepoDetail(document.getElementById('app'), repoName);
    }).catch(function(err) { showToast(err.message, 'error'); });
}

function viewBlob(blobUrl, fileName) {
    var container = document.getElementById('blobView');
    if (!container) return;
    container.innerHTML = '<div class="loading">Loading ' + esc(fileName) + '...</div>';
    apiText(blobUrl).then(function(text) {
        var isLarge = text.length > 100000;
        var display = isLarge ? text.slice(0, 100000) + '\n\n... (truncated, file too large)' : text;
        container.innerHTML = '<div class="card mt-16"><div class="flex-between mb-16">'
            + '<h3>' + esc(fileName) + '</h3>'
            + '<button class="btn btn-sm" id="closeBlobBtn">Close</button></div>'
            + '<pre>' + esc(display) + '</pre></div>';
        document.getElementById('closeBlobBtn').addEventListener('click', function() {
            container.innerHTML = '';
        });
    }).catch(function(err) {
        container.innerHTML = '<div class="error">' + esc(err.message) + '</div>';
    });
}

function viewTree(app, repoRef, treeish, subPath) {
    if (!repoRef) {
        app.innerHTML = '<div class="error">Missing repository reference.</div>';
        return;
    }
    var treeUrl = API + '/repos/tree';
    if (subPath) treeUrl += '/' + subPath.split('/').map(enc).join('/');
    treeUrl += '?ref=' + enc(repoRef) + '&treeish=' + enc(treeish);

    apiJSON(treeUrl).then(function(files) {
        var html = '<div class="breadcrumb">'
            + '<a href="." data-link="/">Repositories</a><span class="sep">/</span>'
            + '<a href="' + escAttr(repoUrl(repoRef)) + '" data-link="' + escAttr(repoUrl(repoRef)) + '">' + esc(repoRef) + '</a>'
            + '<span class="sep">/</span><span class="badge badge-tree">' + esc(treeish) + '</span>';
        if (subPath) {
            var segs = subPath.split('/');
            var acc = '';
            segs.forEach(function(seg, i) {
                acc += (i > 0 ? '/' : '') + seg;
                var link = treePageUrl(repoRef, treeish, acc);
                html += '<span class="sep">/</span><a href="' + escAttr(link) + '" data-link="' + escAttr(link) + '">' + esc(decodeURIComponent(seg)) + '</a>';
            });
        }
        html += '</div>';

        if (subPath) {
            var parent = subPath.split('/').slice(0, -1).join('/');
            var parentLink = treePageUrl(repoRef, treeish, parent);
            html += '<div class="file-entry" data-link="' + escAttr(parentLink) + '">'
                + '<span class="icon">&#8617;</span><span class="name">..</span></div>';
        }

        files.forEach(function(f) {
            if (f.type === 'tree') {
                var link = treePageUrl(repoRef, treeish, (subPath ? subPath + '/' : '') + f.name);
                html += '<div class="file-entry" data-link="' + escAttr(link) + '">'
                    + '<span class="icon">&#128193;</span>'
                    + '<span class="name">' + esc(f.name) + '</span>'
                    + '<span class="hash">' + esc(f.hash.slice(0, 8)) + '</span></div>';
            } else if (f.type === 'commit') {
                html += '<div class="file-entry"><span class="icon">&#128279;</span>'
                    + '<span class="name">' + esc(f.name) + '</span>'
                    + '<span class="hash">submodule</span></div>';
            } else {
                var blobPath = (subPath ? subPath + '/' : '') + f.name;
                var blobUrl = API + '/repos/blob/' + blobPath.split('/').map(enc).join('/') + '?ref=' + enc(repoRef) + '&treeish=' + enc(treeish);
                html += '<div class="file-entry" data-blob-url="' + escAttr(blobUrl) + '" data-blob-name="' + escAttr(f.name) + '">'
                    + '<span class="icon">&#128196;</span>'
                    + '<span class="name">' + esc(f.name) + '</span>'
                    + '<span class="hash">' + esc(f.hash.slice(0, 8)) + '</span></div>';
            }
        });

        html += '<div id="blobView"></div>';
        app.innerHTML = html;
    }).catch(function(err) {
        var link = repoUrl(repoRef);
        app.innerHTML = '<div class="error">' + esc(err.message) + '</div>'
            + '<p><a href="' + escAttr(link) + '" data-link="' + escAttr(link) + '">Back to repository</a></p>';
    });
}

// GitHub 导入页：填账号信息 → 加载仓库 → 勾选 → 生成镜像仓库
var ghState = { repos: [], owner: '', loaded: false };

function ghConflictLabel(c) {
    switch (c) {
        case 'not-a-mirror': return 'name taken by a local repo';
        case 'mirror-same-remote': return 'already mirrored';
        case 'mirror-other-remote': return 'mirrored from another remote';
        case 'alias-exists': return 'alias taken';
        default: return '';
    }
}

function viewImport(app) {
    apiJSON(API + '/repos').then(function(data) {
        var existing = (data.repositories || []).length;
        var html = '<h2 class="mb-16">Import from GitHub</h2>'
            + '<div class="card"><h3>Account</h3>'
            + '<div class="form-group"><label>Owner (user or organization) *</label>'
            + '<input id="ghOwner" placeholder="LaoQi" autocomplete="off" value="' + escAttr(ghState.owner) + '"></div>'
            + '<div class="form-group"><label>Token (optional — without it only public repositories are listed)</label>'
            + '<input id="ghToken" type="password" placeholder="ghp_xxx" autocomplete="off"></div>'
            + '<div class="form-group"><label class="checkbox-label"><input type="checkbox" id="ghForks"> Include forks</label></div>'
            + '<div class="form-group"><label class="checkbox-label"><input type="checkbox" id="ghArchived" checked> Include archived</label></div>'
            + '<div class="form-group"><label>Local name prefix (optional)</label>'
            + '<input id="ghPrefix" placeholder="" autocomplete="off"></div>'
            + '<div class="form-group"><label>Sync interval in seconds (0 = manual only)</label>'
            + '<input id="ghInterval" value="3600" autocomplete="off"></div>'
            + '<details><summary class="muted">Advanced</summary>'
            + '<div class="form-group"><label>API base (default https://api.github.com)</label>'
            + '<input id="ghApiBase" placeholder="https://api.github.com" autocomplete="off"></div>'
            + '<div class="form-group"><label>Clone base (default: the clone URL returned by GitHub)</label>'
            + '<input id="ghCloneBase" placeholder="https://github.com" autocomplete="off"></div>'
            + '<div class="form-group"><label>Proxy (optional)</label>'
            + '<input id="ghProxy" placeholder="http://127.0.0.1:7890" autocomplete="off"></div>'
            + '</details>'
            + '<button class="btn btn-primary btn-sm" id="ghLoadBtn">Load repositories</button> '
            + '<button class="btn btn-sm" id="ghCancelBtn">Cancel</button>'
            + '<div class="muted" style="margin-top:8px">' + existing + ' repositories on this server. '
            + 'Created mirrors are named {prefix}{owner}_{repo} with an additional {owner}/{repo} alias.</div>'
            + '</div><div id="ghList"></div>';
        app.innerHTML = html;

        document.getElementById('ghOwner').value = ghState.owner || '';
        document.getElementById('ghLoadBtn').addEventListener('click', loadGithubRepos);
        document.getElementById('ghCancelBtn').addEventListener('click', function() { navigate('/'); });
        if (ghState.loaded) renderGithubList();
    }).catch(function(err) {
        app.innerHTML = '<div class="error">' + esc(err.message) + '</div>';
    });
}

function ghParam(name) {
    var el = document.getElementById(name);
    return el ? el.value.trim() : '';
}

function loadGithubRepos() {
    var owner = ghParam('ghOwner');
    if (!owner) { showToast('Owner is required', 'error'); return; }
    ghState.owner = owner;
    var params = new URLSearchParams();
    params.set('owner', owner);
    var token = ghParam('ghToken');
    var apiBase = ghParam('ghApiBase');
    var proxy = ghParam('ghProxy');
    var prefix = ghParam('ghPrefix');
    if (apiBase) params.set('apiBase', apiBase);
    if (proxy) params.set('proxy', proxy);
    if (prefix) params.set('namePrefix', prefix);
    if (document.getElementById('ghForks').checked) params.set('includeForks', 'true');
    if (!document.getElementById('ghArchived').checked) params.set('includeArchived', 'false');

    var btn = document.getElementById('ghLoadBtn');
    btn.disabled = true;
    btn.textContent = 'Loading...';
    var headers = token ? { 'X-Github-Token': token } : {};
    fetch(API + '/github/repos?' + params.toString(), { headers: headers })
        .then(function(res) {
            return res.json().catch(function() { return { error: res.statusText }; }).then(function(data) {
                if (!res.ok) throw new Error(data.error || res.statusText);
                return data;
            });
        })
        .then(function(data) {
            ghState.repos = data.repositories || [];
            ghState.loaded = true;
            ghState.token = token;
            renderGithubList();
        })
        .catch(function(err) {
            showToast(err.message, 'error');
            document.getElementById('ghList').innerHTML = '<div class="error">' + esc(err.message) + '</div>';
        })
        .then(function() {
            btn.disabled = false;
            btn.textContent = 'Load repositories';
        });
}

function renderGithubList() {
    var list = document.getElementById('ghList');
    if (!ghState.loaded) { list.innerHTML = ''; return; }
    if (ghState.repos.length === 0) {
        list.innerHTML = '<div class="empty">No repositories found. Check the owner name and token scope.</div>';
        return;
    }
    var creatable = ghState.repos.filter(function(r) { return !r.conflict; }).length;
    var html = '<div class="card"><div class="flex-between mb-16"><h3>Repositories ('
        + ghState.repos.length + ', ' + creatable + ' importable)</h3>'
        + '<span><label class="checkbox-label"><input type="checkbox" id="ghOnlyNew"> only importable</label> '
        + '<input class="search-input" id="ghFilter" placeholder="filter..." autocomplete="off" style="width:160px"></span></div>'
        + '<div id="ghRows"></div>'
        + '<div class="flex-between" style="margin-top:12px">'
        + '<span id="ghCount" class="muted"></span>'
        + '<button class="btn btn-primary btn-sm" id="ghCreateBtn">Create mirror repositories</button></div></div>'
        + '<div id="ghResult"></div>';
    list.innerHTML = html;

    function renderRows() {
        var onlyNew = document.getElementById('ghOnlyNew').checked;
        var q = (document.getElementById('ghFilter').value || '').toLowerCase();
        var rows = '';
        var selected = 0;
        ghState.repos.forEach(function(r, i) {
            var conflict = r.conflict || '';
            if (onlyNew && conflict) return;
            if (q && r.name.toLowerCase().indexOf(q) < 0) return;
            var badges = '';
            if (r.private) badges += ' <span class="badge badge-post">private</span>';
            if (r.fork) badges += ' <span class="badge badge-commit">fork</span>';
            if (r.archived) badges += ' <span class="badge badge-sync-trigger">archived</span>';
            if (conflict) badges += ' <span class="badge badge-sync-fail">' + esc(ghConflictLabel(conflict)) + '</span>';
            if (ghState.selected && ghState.selected[r.name]) selected++;
            rows += '<tr class="gh-row">'
                + '<td style="width:28px"><input type="checkbox" class="gh-check" data-repo="' + escAttr(r.name) + '"'
                + (ghState.selected && ghState.selected[r.name] ? ' checked' : '') + '></td>'
                + '<td><a href="' + escAttr(r.cloneUrl || '#') + '" target="_blank" rel="noopener">' + esc(r.name) + '</a>'
                + badges + '<div class="desc">' + esc(r.description || '') + '</div></td>'
                + '<td style="white-space:nowrap"><code>' + esc(r.localName) + '</code></td>'
                + '<td style="white-space:nowrap">' + (r.defaultBranch ? esc(r.defaultBranch) : '') + '</td>'
                + '<td style="white-space:nowrap">' + esc(fmtBytes((r.sizeKb || 0) * 1024)) + '</td>'
                + '<td style="white-space:nowrap">' + esc(r.updatedAt ? r.updatedAt.slice(0, 10) : '') + '</td>'
                + '</tr>';
        });
        document.getElementById('ghRows').innerHTML = rows
            ? '<table><thead><tr><th></th><th>Repository</th><th>Local name</th><th>Branch</th><th>Size</th><th>Updated</th></tr></thead><tbody>' + rows + '</tbody></table>'
            : '<div class="empty">No matching repositories</div>';
        document.getElementById('ghCount').textContent = selected + ' selected';
    }

    if (!ghState.selected) ghState.selected = {};
    document.getElementById('ghOnlyNew').addEventListener('change', function() {
        if (this.checked) {
            // 默认勾选出所有可导入的仓库
            ghState.repos.forEach(function(r) { if (!r.conflict) ghState.selected[r.name] = true; });
        }
        renderRows();
    });
    document.getElementById('ghFilter').addEventListener('input', renderRows);
    document.getElementById('ghRows').addEventListener('change', function(e) {
        var el = e.target.closest('.gh-check');
        if (!el) return;
        ghState.selected[el.getAttribute('data-repo')] = el.checked;
        var n = 0;
        Object.keys(ghState.selected).forEach(function(k) { if (ghState.selected[k]) n++; });
        document.getElementById('ghCount').textContent = n + ' selected';
    });
    document.getElementById('ghCreateBtn').addEventListener('click', createGithubMirrors);
    renderRows();
}

function createGithubMirrors() {
    var repos = Object.keys(ghState.selected || {}).filter(function(k) { return ghState.selected[k]; });
    if (repos.length === 0) { showToast('Select at least one repository', 'error'); return; }
    var params = new URLSearchParams();
    params.set('owner', ghState.owner);
    var interval = ghParam('ghInterval');
    if (interval) params.set('syncInterval', interval);
    var prefix = ghParam('ghPrefix');
    if (prefix) params.set('namePrefix', prefix);
    var apiBase = ghParam('ghApiBase');
    if (apiBase) params.set('apiBase', apiBase);
    var cloneBase = ghParam('ghCloneBase');
    if (cloneBase) params.set('cloneBase', cloneBase);
    var proxy = ghParam('ghProxy');
    if (proxy) params.set('proxy', proxy);
    repos.forEach(function(r) { params.append('repos', r); });

    var btn = document.getElementById('ghCreateBtn');
    btn.disabled = true;
    btn.textContent = 'Creating...';
    var headers = { 'Content-Type': 'application/x-www-form-urlencoded' };
    var token = ghParam('ghToken') || ghState.token;
    if (token) headers['X-Github-Token'] = token;
    fetch(API + '/github/import', { method: 'POST', headers: headers, body: params })
        .then(function(res) {
            return res.json().catch(function() { return { error: res.statusText }; }).then(function(data) {
                if (!res.ok) throw new Error(data.error || res.statusText);
                return data;
            });
        })
        .then(function(data) {
            showToast(data.created + ' mirror repositories created, ' + data.failed + ' failed', data.failed ? 'error' : '');
            var rows = '';
            (data.results || []).forEach(function(r) {
                var state = r.ok ? '<span class="badge badge-sync-ok">created</span>' : '<span class="badge badge-sync-fail">failed</span>';
                rows += '<tr><td>' + esc(r.repo) + '</td><td><code>' + esc(r.localName || '-') + '</code></td>'
                    + '<td>' + state + ' ' + esc(r.error || r.warning || '') + '</td></tr>';
            });
            document.getElementById('ghResult').innerHTML = '<div class="card"><h3>Result</h3>'
                + '<div class="muted mb-16">' + data.created + ' created, ' + data.failed + ' failed. '
                + 'Initial sync is queued; created mirrors appear on the Repositories page.</div>'
                + '<table><thead><tr><th>Repository</th><th>Local name</th><th>Status</th></tr></thead><tbody>'
                + rows + '</tbody></table></div>';
            document.getElementById('ghCount').textContent = '0 selected';
        })
        .catch(function(err) { showToast(err.message, 'error'); })
        .then(function() {
            btn.disabled = false;
            btn.textContent = 'Create mirror repositories';
        });
}

function viewApiDocs(app) {
    apiJSON(API + '/').then(function(data) {
        var endpoints = data.endpoints || [];
        var html = '<h2 class="mb-16">API Documentation</h2>'
            + '<input class="search-input" id="apiSearch" placeholder="Search endpoints..." autocomplete="off">'
            + '<div class="api-filters">'
            + '<button class="api-filter-btn active" data-filter="ALL">All</button>'
            + '<button class="api-filter-btn" data-filter="GET">GET</button>'
            + '<button class="api-filter-btn" data-filter="POST">POST</button>'
            + '<button class="api-filter-btn" data-filter="DELETE">DELETE</button>'
            + '</div>'
            + '<div id="apiList"></div>';
        app.innerHTML = html;

        var search = document.getElementById('apiSearch');
        var filter = 'ALL';

        function render() {
            var q = search.value.toLowerCase();
            var list = document.getElementById('apiList');
            var html = '';
            endpoints.forEach(function(ep) {
                if (filter !== 'ALL' && ep.method !== filter) return;
                var searchText = (ep.method + ' ' + ep.path + ' ' + ep.summary).toLowerCase();
                if (q && searchText.indexOf(q) < 0) return;

                var badge = 'badge-' + ep.method.toLowerCase();
                html += '<div class="api-endpoint">'
                    + '<div class="header"><span class="badge ' + badge + '">' + ep.method + '</span>'
                    + '<span class="path">' + esc(ep.path) + '</span></div>'
                    + '<div class="summary">' + esc(ep.summary) + '</div>';

                if (ep.params && ep.params.length > 0) {
                    html += '<div class="section-title">Parameters</div><table><thead><tr><th>Name</th><th>In</th><th>Required</th><th>Example</th><th>Description</th></tr></thead><tbody>';
                    ep.params.forEach(function(p) {
                        html += '<tr><td><code>' + esc(p.name) + '</code></td><td>' + esc(p.in) + '</td>'
                            + '<td>' + (p.required ? 'Yes' : 'No') + '</td>'
                            + '<td><code>' + esc(p.example || '') + '</code></td>'
                            + '<td>' + esc(p.desc || '') + '</td></tr>';
                    });
                    html += '</tbody></table>';
                }

                if (ep.requestExample) {
                    html += '<div class="section-title">Request Example</div><pre><code>' + esc(ep.requestExample) + '</code></pre>';
                }
                if (ep.responseExample) {
                    html += '<div class="section-title">Response Example</div><pre><code>' + esc(ep.responseExample) + '</code></pre>';
                }
                if (ep.curl) {
                    html += '<div class="section-title">curl</div><pre><code>' + esc(ep.curl) + '</code></pre>'
                        + '<button class="copy-btn" data-copy="' + escAttr(ep.curl) + '">Copy curl</button>';
                }
                if (ep.notes && ep.notes.length > 0) {
                    html += '<div class="section-title">Notes</div><ul class="notes">';
                    ep.notes.forEach(function(n) { html += '<li>' + esc(n) + '</li>'; });
                    html += '</ul>';
                }
                html += '</div>';
            });
            if (!html) html = '<div class="empty">No matching endpoints</div>';
            list.innerHTML = html;
        }

        search.addEventListener('input', render);
        document.querySelectorAll('.api-filter-btn').forEach(function(btn) {
            btn.addEventListener('click', function() {
                document.querySelectorAll('.api-filter-btn').forEach(function(b) { b.classList.remove('active'); });
                btn.classList.add('active');
                filter = btn.getAttribute('data-filter');
                render();
            });
        });
        render();
    }).catch(function(err) {
        app.innerHTML = '<div class="error">' + esc(err.message) + '</div>';
    });
}
