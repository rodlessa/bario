<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { api, type FeedItem } from '../api'
import { useAuth } from '../auth'
import PostList from '../components/PostList.vue'
import { errorMessage } from '../utils'

const { isAuthenticated } = useAuth()

const items = ref<FeedItem[]>([])
const cursor = ref<string | null>(null)
const pagesLoaded = ref(0)
const loading = ref(false)
const loadingMore = ref(false)
const loaded = ref(false)
const error = ref('')

async function loadFeed(reset: boolean): Promise<void> {
  if (!isAuthenticated.value) {
    return
  }
  if (reset) {
    loading.value = true
  } else {
    loadingMore.value = true
  }
  error.value = ''
  try {
    const page = await api.getFeed(reset ? null : cursor.value, reset ? 0 : pagesLoaded.value)
    items.value = reset ? page.items : [...items.value, ...page.items]
    cursor.value = page.next_cursor
    pagesLoaded.value = reset ? 1 : pagesLoaded.value + 1
    loaded.value = true
  } catch (err) {
    error.value = errorMessage(err, 'Could not load your feed')
  } finally {
    loading.value = false
    loadingMore.value = false
  }
}

onMounted(() => {
  if (isAuthenticated.value) {
    void loadFeed(true)
  }
})

watch(isAuthenticated, (authenticated) => {
  if (authenticated) {
    void loadFeed(true)
  } else {
    items.value = []
    cursor.value = null
    pagesLoaded.value = 0
    loaded.value = false
  }
})
</script>

<template>
  <section class="page-head">
    <div>
      <h1>Home</h1>
      <p class="muted">Barios from the people you follow.</p>
    </div>
    <RouterLink v-if="isAuthenticated" class="button primary" to="/compose">New bario</RouterLink>
  </section>

  <section v-if="!isAuthenticated" class="panel welcome">
    <h2>Your feed lives here</h2>
    <p class="muted">Log in to follow people and read their barios.</p>
    <div class="button-row">
      <RouterLink class="button primary" to="/login">Log in</RouterLink>
      <RouterLink class="button ghost" to="/register">Create account</RouterLink>
    </div>
  </section>

  <template v-else>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="loading" class="panel muted">Loading feed…</p>
    <template v-else>
      <PostList
        :items="items"
        :empty-text="loaded ? 'Your feed is empty. Open a profile and follow someone to fill it.' : 'No barios yet.'"
      />
      <div v-if="cursor" class="load-more">
        <button type="button" class="button" :disabled="loadingMore" @click="loadFeed(false)">
          {{ loadingMore ? 'Loading…' : 'Load more' }}
        </button>
      </div>
    </template>
  </template>
</template>
