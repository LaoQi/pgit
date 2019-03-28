define(["vue", "router", "api"], function (Vue, VueRouter, Api) {
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
            require(["text!/component/settings.html"], function (template) {
                resolve({
                    template: template
                })
            })
        },
        repositories: function(resolve, reject) {
            require(["text!/component/repositories.html"], function (template) {
                resolve({
                    template: template,
                    data: function(){
                        return {
                            repositories: {}
                        }
                    },
                    mounted() {
                        console.log(1)
                        Api.repositories().then(function(data){
                            this.repositories = data
                            console.log(data)
                        }.bind(this))
                    },
                })
            })
        },
        newRepo: function(resolve, reject) {
            require(["text!/component/new.html"], function (template) {
                resolve({
                    template: template,
                    props: ["message"]
                })
            })
        },
    }
})