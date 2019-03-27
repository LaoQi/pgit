define(["vue", "router"], function (Vue, VueRouter) {
    return {
        navbar: function (resolve, reject) {
            require(["text!/component/navbar.html"], function (template) {
                resolve({
                    template: template,
                    props: ["message"]
                })
            })
        },
        dashboard: function (resolve, reject) {
            require(["text!/component/dashboard.html"], function (template) {
                resolve({
                    template: template
                })
            })
        },
        settings: function(resolve, reject) {
            require(["text!/component/dashboard.html"], function (template) {
                resolve({
                    template: template
                })
            })
        },
        repositories: function(resolve, reject) {
            require(["text!/component/repositories.html"], function (template) {
                resolve({
                    template: template
                })
            })
        },
    }
})