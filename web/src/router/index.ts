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
      { path: 'hosts', name: 'hosts', component: () => import('../views/hosts/HostList.vue') },
      { path: 'hosts/:id', name: 'host-detail', component: () => import('../views/hosts/HostDetail.vue') },
      { path: 'batch-exec', name: 'batch-exec', component: () => import('../views/exec/BatchExec.vue') },
      { path: 'files/:id', name: 'file-manager', component: () => import('../views/files/FileManager.vue') },
      { path: 'sessions', name: 'sessions', component: () => import('../views/audit/SessionList.vue') },
      { path: 'alerts', name: 'alerts', component: () => import('../views/alerts/AlertList.vue') },
      { path: 'vault', name: 'vault', component: () => import('../views/vault/CredentialList.vue') },
      { path: 'docker/:id', name: 'docker', component: () => import('../views/docker/Docker.vue') },
      { path: 'users', name: 'users', component: () => import('../views/users/UserList.vue') },
      { path: 'settings', name: 'settings', component: () => import('../views/settings/Settings.vue') },
      { path: 'ai/chat', name: 'ai-chat', component: () => import('../views/ai/AiChat.vue') },
      { path: 'ai/report', name: 'ai-report', component: () => import('../views/ai/OpsReport.vue') },
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