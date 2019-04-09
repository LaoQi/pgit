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
                    template: template,
                    props: ["message"],
                    data(){
                        return {
                            total: 0,
                            commits: 0,
                        }
                    },
                    activated() {
                        Api.dashboard().then(
                            data => {
                                this.total = data.total
                            }
                        )
                    },
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
                                    this.reverse ? (a[this.sortBy] < b[this.sortBy] ? 1 : -1) : (a[this.sortBy] > b[this.sortBy] ? 1 : -1)
                                )
                        },
                    },
                    activated() {
                        Api.explore().then(function (data) {
                            this.repositories = data.repositories.sort(function(a, b){
                                return a.name > b.name ? 1 : -1
                            })
                        }.bind(this))
                    },
                })
            })
        },
        repository: function(resolve, reject) {
            require(["text!/component/repository.html", "marked", "clipboard"], function (template, marked, Clipboard) {
                resolve({
                    template: template,
                    props: ["message", "name"],
                    data () {
                        return {
                            metadata: {
                                name: "",
                                description: ""
                            },
                            empty: false,
                            download: "",
                            tree: [],
                            branch: "master",
                            paths: [],
                            readme: "",
                            cloneType: "http",
                        }
                    },
                    computed: {
                        subpath(){
                            if (this.paths.length > 0) {
                                return this.paths.join("/") + "/"
                            }
                            return ""
                        },
                        address() {
                            return this.cloneType == "ssh" ? 
                            "ssh://" + window.location.host + "/" + this.metadata.name + ".git"  :
                            "http://" + window.location.host + "/repo/" + this.metadata.name + ".git"
                        },
                        helpCreateCommand() {
                            return "touch README.md\ngit init\ngit add README.md\ngit commit -m \"first commit\"\ngit remote add origin " +
                            this.address + "\ngit push -u origin master"
                        },
                        helpPushCommand() {
                            return "git remote add origin " + this.address + "\ngit push -u origin master"
                        }
                    },
                    activated() {
                        var clipboard = new Clipboard(".clipboard-button")
                        clipboard.on('success', function(e) {
                            console.info('Trigger:', e.trigger);  
                            e.clearSelection();
                        });
                        this.download = "/repo/" + this.name + "/archive/" + this.branch
                        this.paths = []
                        this.metadata = { name: "", description: "" }
                        this.tree = []
                        this.readme = ""
                        Api.repository(this.name).then( data => this.metadata = data.metadata)
                        this.updateTree()
                    },
                    methods: {
                        next(node) {
                            if (node.type === "tree") {
                                this.paths.push(node.name)
                                this.updateTree()
                            }
                        },
                        prev(name) {
                            if (typeof name === "undefined") {
                                this.paths.pop()
                            } else {
                                var index = this.paths.indexOf(name)
                                this.paths = this.paths.slice(0,  index + 1)
                            }
                            this.updateTree()
                        },
                        updateTree() {
                            // this.tree = []
                            Api.tree(this.name, this.subpath).then(
                                data => {
                                    this.empty = data.length === 0
                                    this.readme = ""
                                    this.tree = data.sort((a, b) => a.type < b.type ? 1 : -1)
                                    this.updateReadme()
                                }
                            ).catch(
                                reason => {
                                    this.empty = true
                                }
                            )
                        },
                        updateReadme() {
                            var index = this.tree.findIndex(
                                item => item.name.toLowerCase() == "readme.md" || item.name.toLowerCase() == "readme.markdown"
                            )
                            if (index > -1) {
                                Api.blob(this.name, this.subpath + this.tree[index].name).then(
                                    data => this.readme = marked(data)
                                )
                            }
                        }
                    }
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