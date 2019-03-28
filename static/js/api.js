define([], function () {
    return {
        repositories() {
            return fetch("/repo", {
                method: 'GET',
            }).then(response => response.json())
        }
    }
})