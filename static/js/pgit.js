import Vue from '/js/vue.v2.6.10.esm.browser.min.js'
import VueRouter from '/js/vue-router.v3.1.3.esm.browser.min.js'
import httpVueLoader from '/js/httpVueLoader.js'
import Lang from '/js/lang.js'

Vue.use(VueRouter);
Vue.use(httpVueLoader);
Vue.component("Navbar", 'url:/component/navbar.vue');
Vue.component("App", "url:/component/app.vue");
// Vue.component("HighlightBlock", component.highlightBlock);

const router = new VueRouter({
    mode: 'history',
    routes: [
        { path: '/dashboard', component: 'url:/component/dashboard.vue' },
        // { path: '/settings', component: component.settings },
        // { path: '/repositories', component: component.repositories, alias: '/' },
        // { path: '/repository/:name', component: component.repository, props: true },
        // { path: '/new', component: component.newRepo },
    ]
});
window._App = new Vue({
    el: "#app",
    router,
    data: {
        message: Lang.message
    },
    template: '<App></App>'
});
