define(["vue", "router", "api"], function (Vue, VueRouter, Api) {
    return {
        navbar: function (resolve, reject) {
            require(["text!/component/navbar.html"], function (template) {
                resolve({
                    template: template,
                    props: ["message"],
                    data(){
                        return {
                            menuActive: false,
                        }
                    },
                    methods: {
                        toggleMenu() {
                            this.menuActive = !this.menuActive
                        },
                        closeMenu() {
                            this.menuActive = false
                        }
                    }
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
        settings: function (resolve, reject) {
            require(["text!/component/settings.html"], function (template) {
                resolve({
                    template: template
                })
            })
        },
        repositories: function (resolve, reject) {
            require(["text!/component/repositories.html"], function (template) {
                resolve({
                    template: template,
                    props: ["message"],
                    data () {
                        return {
                            repositories: [],
                            sortBy: "name",
                            reverse: false,
                            keywords: "",
                        }
                    },
                    computed: {
                        sorted(){
                            return this.repositories
                                .filter((item, index) =>
                                    item.name.indexOf(this.keywords) > -1 || item.description.indexOf(this.keywords) > -1
                                )
                                .sort((a, b) =>
                                    this.reverse ? a[this.sortBy] < b[this.sortBy] : a[this.sortBy] > b[this.sortBy]
                                )
                        }
                    },
                    activated() {
                        Api.repositories().then(function (data) {
                            this.repositories = data.sort(function(a, b){
                                return a.name > b.name
                            })
                        }.bind(this))
                    },
                })
            })
        },
        repository: function(resolve, reject) {
            require(["text!/component/repository.html"], function (template) {
                resolve({
                    template: template,
                    props: ["message", "name"],
                    data () {
                        return {
                            metadata: {
                                name: "",
                                description: ""
                            },
                            download: "",
                            tree: [],
                        }
                    },
                    activated() {
                        this.download = "/repo/" + this.name + "/archive/master"
                        Api.repository(this.name).then(function (data) {
                            this.metadata = data.metadata
                        }.bind(this))
                    },
                })
            })
        },
        newRepo: function (resolve, reject) {
            require(["text!/component/new.html"], function (template) {
                resolve({
                    template: template,
                    props: ["message"],
                    data() {
                        return {
                            name: "",
                            description: "",
                            readme: false,
                            gitignore: "None",
                            license: "None"
                        }
                    },
                    activated(){
                        this.name = this.description = "",
                        this.readme = false, this.gitignore = "None", this.license = "None"
                    },
                    methods:{
                        submit(){
                            console.log(this.name, this.description, this.readme, this.gitignore, this.license)
                            Api.create(
                                this.name, this.description, this.readme, 
                                this.gitignore, this.license
                            ).then(response => {
                                console.log(response)
                                this.$router.push('/repositories')
                            })
                        }
                    }
                })
            })
        }
    }
})