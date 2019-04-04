requirejs.config({
    //By default load any module IDs from js/lib
    baseUrl: '/',
    //except, if the module ID starts with "app",
    //load it from the js/app directory. paths
    //config is relative to the baseUrl, and
    //never includes a ".js" extension since
    //the paths config could be for a directory.
    paths: {
        text: "/js/text.v2.0.12.min",
        vue: "/js/vue.v2.6.10.min",
        router: "/js/vue-router.v3.0.2.min",
        lang: "/js/lang",
        api: "/js/api",
        component: "/js/component",
    },
    waitSeconds: 0
});

require(["vue", "router", "lang", "component"], function(Vue, VueRouter, lang, component){
    
    Vue.use(VueRouter)
    Vue.component("Navbar", component.navbar)

    const router = new VueRouter({
        routes: [
            { path: '/dashboard', component: component.dashboard, alias: '/' },
            { path: '/settings', component: component.settings },
            { path: '/repositories', component: component.repositories },
            { path: '/repository/:name', component: component.repository, props: true },
            { path: '/new', component: component.newRepo },
        ]
    }) 
    App = new Vue({
        el: "#app",
        router,
        data: {
            message: lang.message
        },
    })
})
