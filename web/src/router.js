import { createRouter, createWebHashHistory } from 'vue-router'
import { session, loadSession } from './api'

const Layout = () => import('./layout/Layout.vue')

const routes = [
  { path: '/setup', component: () => import('./views/Setup.vue'), meta: { public: true } },
  { path: '/login', component: () => import('./views/Login.vue'), meta: { public: true } },
  { path: '/status/:slug', component: () => import('./views/Monitor.vue'), props: r => ({ slug: r.params.slug }), meta: { public: true, standalone: true } },
  {
    path: '/',
    component: Layout,
    children: [
      { path: '', redirect: '/dashboard' },
      { path: 'dashboard', component: () => import('./views/Dashboard.vue'), meta: { title: ['仪表盘'] } },
      { path: 'monitor', component: () => import('./views/Monitor.vue'), meta: { title: ['资源监控'] } },
      { path: 'rules', component: () => import('./views/Rules.vue'), meta: { title: ['转发管理', '转发规则'] } },
      { path: 'web-settings', component: () => import('./views/WebSettings.vue'), meta: { title: ['转发管理', 'Web 转发设置'], admin: true } },
      { path: 'users', component: () => import('./views/Users.vue'), meta: { title: ['用户管理', '用户列表'], admin: true } },
      { path: 'users/new', component: () => import('./views/UserNew.vue'), meta: { title: ['用户管理', '添加用户'], admin: true } },
      { path: 'users/:id', component: () => import('./views/UserEdit.vue'), props: true, meta: { title: ['用户管理', '编辑用户'], admin: true, menu: '/users' } },
      { path: 'nodes', component: () => import('./views/Nodes.vue'), meta: { title: ['PVE 节点', '节点列表'], admin: true } },
      { path: 'nodes/new', component: () => import('./views/NodeNew.vue'), meta: { title: ['PVE 节点', '添加节点'], admin: true } },
      { path: 'nodes/:id/setup', component: () => import('./views/NodeNew.vue'), props: true, meta: { title: ['PVE 节点', '节点接入'], admin: true, menu: '/nodes' } },
      { path: 'nodes/:id', component: () => import('./views/NodeEdit.vue'), props: true, meta: { title: ['PVE 节点', '节点配置'], admin: true, menu: '/nodes' } },
      { path: 'vms', component: () => import('./views/VMs.vue'), meta: { title: ['PVE 节点', '虚拟机'], admin: true } },
      { path: 'credentials', component: () => import('./views/Credentials.vue'), meta: { title: ['PVE 节点', 'SSH 凭证'], admin: true } },
      { path: 'settings', component: () => import('./views/Settings.vue'), meta: { title: ['系统设置'], admin: true } },
      { path: 'profile', component: () => import('./views/Profile.vue'), meta: { title: ['个人中心', '个人资料'] } },
    ],
  },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' },
]

const router = createRouter({ history: createWebHashHistory(), routes })

router.beforeEach(async to => {
  if (!session.loaded) {
    try { await loadSession() } catch { /* 网络错误时仍允许进入登录页 */ }
  }
  if (to.meta.standalone) return true
  if (!session.setup && to.path !== '/setup') return '/setup'
  if (session.setup && to.path === '/setup') return '/login'
  if (to.meta.public) {
    if (to.path === '/login' && session.user) return '/dashboard'
    return true
  }
  if (!session.user) return { path: '/login', query: { redirect: to.fullPath } }
  if (to.meta.admin && session.user.role !== 'admin') return '/dashboard'
  return true
})

export default router
