import { createRouter, createWebHistory, type RouteRecordRaw, type RouteLocationNormalized } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('../views/auth/Login.vue'),
    meta: { public: true },
  },
  {
    path: '/share/terminal',
    name: 'shared-terminal',
    component: () => import('../views/terminal/SharedTerminal.vue'),
    meta: { public: true },
  },
  {
    path: '/',
    component: () => import('../components/layout/AppShell.vue'),
    children: [
      { path: '', redirect: '/hosts' },
      // meta.viewName 是该视图的组件名，AppShell 据此决定 KeepAlive 缓存哪些
      // 页面（见 AGENTS.md 8.4）。必须与组件里 defineOptions 的 name 一致。
      { path: 'hosts', name: 'hosts', component: () => import('../views/hosts/HostList.vue'), meta: { viewName: 'HostList' } },
      { path: 'hosts/:id', name: 'host-detail', component: () => import('../views/hosts/HostDetail.vue'), meta: { viewName: 'HostDetail' } },
      { path: 'batch-exec', name: 'batch-exec', component: () => import('../views/exec/BatchExec.vue'), meta: { viewName: 'BatchExec' } },
      { path: 'files/:id', name: 'file-manager', component: () => import('../views/files/FileManager.vue'), meta: { viewName: 'FileManager' } },
      { path: 'sessions', name: 'sessions', component: () => import('../views/audit/SessionList.vue'), meta: { viewName: 'SessionList' } },
      { path: 'audit', name: 'audit', component: () => import('../views/audit/AuditList.vue'), meta: { viewName: 'AuditList' } },
      { path: 'alerts', name: 'alerts', component: () => import('../views/alerts/AlertList.vue'), meta: { viewName: 'AlertList' } },
      { path: 'vault', name: 'vault', component: () => import('../views/vault/CredentialList.vue'), meta: { viewName: 'CredentialList' } },
      { path: 'docker/:id', name: 'docker', component: () => import('../views/docker/Docker.vue'), meta: { viewName: 'Docker' } },
      { path: 'groups', name: 'groups', component: () => import('../views/groups/GroupList.vue'), meta: { viewName: 'GroupList' } },
      { path: 'users', name: 'users', component: () => import('../views/users/UserList.vue'), meta: { viewName: 'UserList' } },
      { path: 'settings', name: 'settings', component: () => import('../views/settings/Settings.vue'), meta: { viewName: 'Settings' } },
      { path: 'ai/chat', name: 'ai-chat', component: () => import('../views/ai/AiChat.vue'), meta: { viewName: 'AiChat' } },
      { path: 'ai/report', name: 'ai-report', component: () => import('../views/ai/OpsReport.vue'), meta: { viewName: 'OpsReport' } },
    ],
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach((to: RouteLocationNormalized) => {
  const auth = useAuthStore()
  if (!to.meta.public && !auth.isLoggedIn) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && auth.isLoggedIn) {
    return { name: 'hosts' }
  }
})

export default router