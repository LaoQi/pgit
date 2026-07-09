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
    if (path === '/') {
        viewRepos(app);
    } else if (path.indexOf('/repo/') === 0) {
        var rest = path.slice('/repo/'.length);
        var parts = rest.split('/');
        var name = decodeURIComponent(parts[0]);
        if (parts.length >= 3 && parts[1] === 'tree') {
            var ref = decodeURIComponent(parts[2]);
            var subPath = parts.slice(3).map(decodeURIComponent).join('/');
            viewTree(app, name, ref, subPath);
        } else {
            viewRepoDetail(app, name);
        }
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
            + '</div>'
            + '<button class="btn btn-primary btn-sm" id="createRepoBtn">Create</button>'
            + '</div></div>';

        if (repos.length === 0) {
            html += '<div class="empty">No repositories yet. Create one to get started.</div>';
        } else {
            html += '<div class="repo-grid">';
            repos.forEach(function(r) {
                var aliases = r.aliases || [];
                var firstAlias = aliases[0] || r.name;
                var host = window.location.host;
                var httpClone = window.location.protocol + '//' + host + '/' + firstAlias + '.git';
                var sshClone = 'ssh://' + host + '/' + firstAlias + '.git';
                var link = '/repo/' + enc(r.name);
                var mirrorBadge = r.mirror ? ' <span class="badge badge-mirror">mirror</span>' : '';
                html += '<div class="repo-card">'
                    + '<div class="name"><a href="repo/' + enc(r.name) + '" data-link="' + escAttr(link) + '">' + esc(r.name) + '</a>' + mirrorBadge + '</div>'
                    + '<div class="desc">' + esc(r.description || 'No description') + '</div>'
                    + '<div class="meta"><span>aliases: ' + aliases.length + '</span><span>' + esc(fmtDate(r.createdAt)) + '</span></div>'
                    + '<div class="clone-box"><span class="label">HTTP</span><span class="url">' + esc(httpClone) + '</span>'
                    + '<button class="copy-btn" data-copy="' + escAttr(httpClone) + '">copy</button></div>'
                    + '<div class="clone-box"><span class="label">SSH</span><span class="url">' + esc(sshClone) + '</span>'
                    + '<button class="copy-btn" data-copy="' + escAttr(sshClone) + '">copy</button></div>'
                    + '</div>';
            });
            html += '</div>';
        }
        app.innerHTML = html;

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
                var params = { description: desc, mirrorUrl: mirrorUrl };
                var interval = document.getElementById('mirrorInterval').value.trim();
                if (interval) params.mirrorInterval = interval;
                var authType = document.getElementById('mirrorAuthType').value;
                params.mirrorAuthType = authType;
                if (authType === 'basic') {
                    params.mirrorUsername = document.getElementById('mirrorUsername').value.trim();
                    params.mirrorPassword = document.getElementById('mirrorPassword').value;
                }
                apiForm('POST', API + '/repos/' + enc(name), params).then(function() {
                    showToast('Mirror repository created, sync will start shortly');
                    navigate('/repo/' + enc(name));
                }).catch(function(err) { showToast(err.message, 'error'); });
            } else {
                var defaultBranch = document.getElementById('repoDefaultBranch').value.trim() || 'master';
                apiForm('POST', API + '/repos/' + enc(name), { description: desc, defaultBranch: defaultBranch }).then(function() {
                    showToast('Repository created');
                    navigate('/repo/' + enc(name));
                }).catch(function(err) { showToast(err.message, 'error'); });
            }
        });
    }).catch(function(err) {
        app.innerHTML = '<div class="error">' + esc(err.message) + '</div>';
    });
}

function viewRepoDetail(app, name) {
    apiJSON(API + '/repos/' + enc(name)).then(function(data) {
        var repo = data.metadata || {};
        var refs = data.refs || [];
        var aliases = repo.aliases || [];
        var host = window.location.host;
        var repoLink = '/repo/' + enc(repo.name);

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
             html += '<div class="card"><h3>Mirror</h3>'
                 + '<p class="text-sm text-muted">Remote: <code>' + esc(m.remoteUrl) + '</code></p>'
                 + '<p class="text-sm text-muted mt-4">Sync: ' + esc(intervalText) + '</p>'
                 + '<p class="text-sm text-muted mt-4">Auth: ' + esc(authText) + '</p>'
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
                 var treeLink = '/repo/' + enc(repo.name) + '/tree/' + enc(ref.name);
                 html += '<tr><td>' + badge + '</td><td>' + esc(ref.name) + '</td>'
                     + '<td>' + esc(ref.subject || '') + '</td>'
                     + '<td>' + esc(ref.author || '') + '</td>'
                     + '<td class="text-muted">' + esc(fmtTs(ref.timestamp)) + '</td>'
                     + '<td><a href="repo/' + enc(repo.name) + '/tree/' + enc(ref.name) + '" data-link="' + escAttr(treeLink) + '" class="btn btn-sm">Browse</a></td>'
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
             var hasSlash = a.indexOf('/') >= 0;
             html += '<div class="alias-item"><span class="alias-name">' + esc(a) + '</span>';
             if (a === repo.name) {
                 html += '<span class="text-muted text-sm">(default)</span>';
             } else if (hasSlash) {
                 html += '<span class="alias-note">Cannot delete via API (contains slash)</span>';
             } else {
                 html += '<button class="btn btn-danger btn-sm" data-remove-repo="' + escAttr(repo.name) + '" data-remove-alias="' + escAttr(a) + '">Remove</button>';
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

         html += '<div class="card"><h3>Danger Zone</h3>'
             + '<p class="text-muted text-sm">Delete this repository. This action cannot be undone.</p>'
             + '<button class="btn btn-danger btn-sm" id="deleteRepoBtn">Delete Repository</button></div>';

         app.innerHTML = html;

        if (document.getElementById('downloadArchiveBtn')) {
            document.getElementById('downloadArchiveBtn').addEventListener('click', function() {
                var ref = document.getElementById('archiveRef').value;
                window.open(API + '/repos/' + enc(repo.name) + '/archive/' + enc(ref), '_blank');
            });
        }
        document.getElementById('addAliasBtn').addEventListener('click', function() {
            var alias = document.getElementById('newAlias').value.trim();
            if (!alias) { showToast('Alias is required', 'error'); return; }
            apiForm('POST', API + '/repos/' + enc(repo.name) + '/aliases', { alias: alias }).then(function() {
                showToast('Alias added');
                viewRepoDetail(app, name);
            }).catch(function(err) { showToast(err.message, 'error'); });
        });
         document.getElementById('deleteRepoBtn').addEventListener('click', function() {
             var input = prompt('Type the repository name to confirm deletion:', '');
             if (input !== repo.name) { showToast('Confirmation mismatch', 'error'); return; }
             apiDelete(API + '/repos/' + enc(repo.name) + '?confirm=' + enc(repo.name)).then(function() {
                 showToast('Repository deleted');
                 navigate('/');
             }).catch(function(err) { showToast(err.message, 'error'); });
         });
          if (document.getElementById('setDefaultBranchBtn')) {
              document.getElementById('setDefaultBranchBtn').addEventListener('click', function() {
                  var branch = document.getElementById('newDefaultBranch').value;
                  if (!branch) { showToast('Select a branch', 'error'); return; }
                  apiForm('POST', API + '/repos/' + enc(repo.name) + '/default-branch', { branch: branch }).then(function() {
                      showToast('Default branch updated to ' + branch);
                      viewRepoDetail(app, name);
                  }).catch(function(err) { showToast(err.message, 'error'); });
              });
          }

         var commitsContainer = document.getElementById('commitsList');
         if (commitsContainer) {
             var commitsRef = data.defaultBranch || 'master';
             apiJSON(API + '/repos/' + enc(repo.name) + '/commits/' + enc(commitsRef) + '?limit=20').then(function(commits) {
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
                 apiForm('POST', API + '/repos/' + enc(repo.name) + '/sync').then(function(data) {
                     var sync = data.sync || {};
                     if (sync.success) {
                         showToast('Sync completed: ' + sync.objectsFetched + ' objects, ' + sync.refsUpdated + ' refs updated');
                     } else {
                         showToast('Sync failed: ' + (sync.error || 'unknown error'), 'error');
                     }
                     viewRepoDetail(app, name);
                 }).catch(function(err) {
                     showToast(err.message, 'error');
                     syncNowBtn.disabled = false;
                     syncNowBtn.textContent = 'Sync Now';
                 });
             });
         }

         var syncLogContainer = document.getElementById('syncLogList');
         if (syncLogContainer) {
             apiJSON(API + '/repos/' + enc(repo.name) + '/sync-log?limit=20').then(function(data) {
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
    apiDelete(API + '/repos/' + enc(repoName) + '/aliases/' + enc(alias)).then(function() {
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

function viewTree(app, name, ref, subPath) {
    var treeUrl = API + '/repos/' + enc(name) + '/tree/' + enc(ref) + '/';
    if (subPath) treeUrl += subPath.split('/').map(enc).join('/');

    apiJSON(treeUrl).then(function(files) {
        var html = '<div class="breadcrumb">'
            + '<a href="." data-link="/">Repositories</a><span class="sep">/</span>'
            + '<a href="repo/' + enc(name) + '" data-link="/repo/' + enc(name) + '">' + esc(name) + '</a>'
            + '<span class="sep">/</span><span class="badge badge-tree">' + esc(ref) + '</span>';
        if (subPath) {
            var segs = subPath.split('/');
            var acc = '';
            segs.forEach(function(seg, i) {
                acc += (i > 0 ? '/' : '') + seg;
                var link = '/repo/' + enc(name) + '/tree/' + enc(ref) + '/' + acc.split('/').map(enc).join('/');
                html += '<span class="sep">/</span><a href="repo/' + enc(name) + '/tree/' + enc(ref) + '/' + acc.split('/').map(enc).join('/') + '" data-link="' + escAttr(link) + '">' + esc(decodeURIComponent(seg)) + '</a>';
            });
        }
        html += '</div>';

        if (subPath) {
            var parent = subPath.split('/').slice(0, -1).join('/');
            var parentLink = '/repo/' + enc(name) + '/tree/' + enc(ref);
            if (parent) parentLink += '/' + parent.split('/').map(enc).join('/');
            html += '<div class="file-entry" data-link="' + escAttr(parentLink) + '">'
                + '<span class="icon">&#8617;</span><span class="name">..</span></div>';
        }

        files.forEach(function(f) {
            if (f.type === 'tree') {
                var link = '/repo/' + enc(name) + '/tree/' + enc(ref) + '/' + (subPath ? subPath.split('/').map(enc).join('/') + '/' : '') + enc(f.name);
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
                var blobUrl = API + '/repos/' + enc(name) + '/blob/' + enc(ref) + '/' + blobPath.split('/').map(enc).join('/');
                html += '<div class="file-entry" data-blob-url="' + escAttr(blobUrl) + '" data-blob-name="' + escAttr(f.name) + '">'
                    + '<span class="icon">&#128196;</span>'
                    + '<span class="name">' + esc(f.name) + '</span>'
                    + '<span class="hash">' + esc(f.hash.slice(0, 8)) + '</span></div>';
            }
        });

        html += '<div id="blobView"></div>';
        app.innerHTML = html;
    }).catch(function(err) {
        var link = '/repo/' + enc(name);
        app.innerHTML = '<div class="error">' + esc(err.message) + '</div>'
            + '<p><a href="repo/' + enc(name) + '" data-link="' + escAttr(link) + '">Back to repository</a></p>';
    });
}

function viewApiDocs(app) {
    apiJSON(API + '/').then(function(data) {
        var endpoints = data.endpoints || [];
        var html = '<h2 class="mb-16">API Documentation</h2>'
            + '<input class="api-search" id="apiSearch" placeholder="Search endpoints..." autocomplete="off">'
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
