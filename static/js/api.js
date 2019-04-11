define([], function () {
    return {
        explore() {
            return fetch("/repo", {
                method: 'GET',
            }).then(response => response.json())
        },

        repository(name) {
            return fetch("/repo/" + name, {
                method: 'GET',
            }).then(response => response.json())
        },
        tree(name, ref, path) {
            subpath = path ? path : ""
            var url = "/repo/" + encodeURIComponent(name) + "/tree/" + encodeURIComponent(ref) + "/" + subpath;
            return fetch(url, {
                method: 'GET',
            }).then(response => response.json())
        },
        blob(name, ref, path) {
            var url = "/repo/" + encodeURIComponent(name) + "/blob/" + encodeURIComponent(ref) + "/" + path;
            return fetch(url, {
                method: 'GET',
            }).then(response => response.text())
        },
        create(name, description, readme, gitignore, license) {
            var body = new URLSearchParams()
            body.append('name', name)
            body.append('description', description)
            body.append('readme', readme)
            body.append('gitignore', gitignore)
            body.append('license', license)
            return fetch("/repo/" + name, {
                method: 'POST',
                headers: {
                    'content-type': 'application/x-www-form-urlencoded'
                },
                body: body
            })
        }
    }
})