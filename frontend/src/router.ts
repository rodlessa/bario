import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useAuth } from './auth'

declare module 'vue-router' {
  interface RouteMeta {
    title?: string
    requiresAuth?: boolean
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'feed',
    component: () => import('./views/FeedView.vue'),
    meta: { title: 'Home' },
  },
  {
    path: '/login',
    name: 'login',
    component: () => import('./views/LoginView.vue'),
    meta: { title: 'Log in' },
  },
  {
    path: '/register',
    name: 'register',
    component: () => import('./views/RegisterView.vue'),
    meta: { title: 'Register' },
  },
  {
    path: '/compose',
    name: 'compose',
    component: () => import('./views/ComposeView.vue'),
    meta: { title: 'Compose', requiresAuth: true },
  },
  {
    path: '/u/:id',
    name: 'profile',
    component: () => import('./views/ProfileView.vue'),
    meta: { title: 'Profile' },
  },
  {
    path: '/post/:id',
    name: 'post',
    component: () => import('./views/PostView.vue'),
    meta: { title: 'Post' },
  },
  {
    path: '/:pathMatch(.*)*',
    redirect: '/',
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach((to) => {
  const { isAuthenticated } = useAuth()
  if (to.meta.requiresAuth && !isAuthenticated.value) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  return true
})

router.afterEach((to) => {
  document.title = to.meta.title ? `${to.meta.title} · Bario` : 'Bario'
})

export default router
