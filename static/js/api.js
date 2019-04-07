define([], function () {
    return {
        repositories() {
            return fetch("/repo", {
                method: 'GET',
            }).then(response => response.json())
        },

        repository(name) {
            return fetch("/repo/" + name, {
                method: 'GET',
            }).then(response => response.json())
        },
        tree(name, path) {
            subpath = path ? path : ""
            return fetch("/repo/" + name + "/tree/master/" + subpath, {
                method: 'GET',
            }).then(response => response.json())
        },
        blob(name, path) {
            return fetch("/repo/" + name + "/blob/master/" + path, {
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