<script setup lang="ts">
import { RouterLink, RouterView, useRouter } from 'vue-router'
import { useAuth } from './auth'
import { useLayout } from './layout'

const { state, isAuthenticated, logout } = useAuth()
const { layout, setLayout } = useLayout()
const router = useRouter()

function handleLogout(): void {
  logout()
  router.push({ name: 'feed' })
}
</script>

<template>
  <div class="app" :class="`layout-${layout}`">
    <header class="topbar">
      <div class="topbar-inner">
        <RouterLink class="brand" to="/">chirp<span class="brand-mark">.</span></RouterLink>

        <nav class="topnav">
          <RouterLink to="/">Feed</RouterLink>
          <RouterLink v-if="isAuthenticated" to="/compose">Compose</RouterLink>
          <RouterLink v-if="state.user" :to="`/u/${state.user.id}`">@{{ state.user.username }}</RouterLink>
        </nav>

        <div class="topbar-actions">
          <div class="layout-toggle" role="group" aria-label="Layout style">
            <button
              type="button"
              :class="{ active: layout === 'classic' }"
              :aria-pressed="layout === 'classic'"
              @click="setLayout('classic')"
            >
              Classic
            </button>
            <button
              type="button"
              :class="{ active: layout === 'modern' }"
              :aria-pressed="layout === 'modern'"
              @click="setLayout('modern')"
            >
              Modern
            </button>
          </div>

          <template v-if="isAuthenticated">
            <button type="button" class="button ghost" @click="handleLogout">Log out</button>
          </template>
          <template v-else>
            <RouterLink class="button ghost" to="/login">Log in</RouterLink>
            <RouterLink class="button primary" to="/register">Join</RouterLink>
          </template>
        </div>
      </div>
    </header>

    <div class="shell">
      <aside class="sidebar">
        <div class="sidebar-card account">
          <template v-if="state.user">
            <RouterLink class="account-name" :to="`/u/${state.user.id}`">@{{ state.user.username }}</RouterLink>
            <p class="muted">Signed in</p>
            <RouterLink class="button primary block" to="/compose">New chirp</RouterLink>
          </template>
          <template v-else>
            <p class="sidebar-title">Welcome to Chirp</p>
            <p class="muted">Short messages, images, and the people you follow.</p>
            <RouterLink class="button primary block" to="/register">Create account</RouterLink>
            <RouterLink class="button ghost block" to="/login">Log in</RouterLink>
          </template>
        </div>

        <nav class="sidebar-card sidebar-nav" aria-label="Site">
          <RouterLink to="/">Home feed</RouterLink>
          <RouterLink v-if="isAuthenticated" to="/compose">Compose</RouterLink>
          <RouterLink v-if="state.user" :to="`/u/${state.user.id}`">My profile</RouterLink>
        </nav>

        <div class="sidebar-card about">
          <p class="sidebar-title">Layout</p>
          <p class="muted">
            Classic is a dense, old-school list. Modern is a roomy card view. Your choice is saved in this
            browser.
          </p>
        </div>
      </aside>

      <main class="content">
        <RouterView />
      </main>
    </div>
  </div>
</template>
