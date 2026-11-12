<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { api, type FeedItem } from '../api'
import { useAuth } from '../auth'
import PostList from '../components/PostList.vue'
import { errorMessage } from '../utils'

const FEED_LIMIT = 30
const SENTINEL_MARGIN = '150px 0px'

const { isAuthenticated } = useAuth()

const items = ref<FeedItem[]>([])
const sentinelStarts = ref<number[]>([])
const cursor = ref<string | null>(null)
const pagesLoaded = ref(0)
const loading = ref(false)
const loadingMore = ref(false)
const loaded = ref(false)
const error = ref('')
const loadError = ref(false)

const feedRoot = ref<HTMLElement | null>(null)
const retrySentinel = ref<HTMLElement | null>(null)

let generation = 0
let sentinelObserver: IntersectionObserver | null = null
let retryScrollHandler: (() => void) | null = null
let retryArmed = false

function removeRetryScrollHandler(): void {
  if (retryScrollHandler) {
    window.removeEventListener('scroll', retryScrollHandler)
    retryScrollHandler = null
  }
}

function ensureSentinelObserver(): IntersectionObserver {
  if (sentinelObserver) {
    return sentinelObserver
  }
  sentinelObserver = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) {
          continue
        }
        const element = entry.target as HTMLElement
        if (element.dataset.retry === 'true') {
          if (!retryArmed) {
            continue
          }
          retryArmed = false
          sentinelObserver?.unobserve(element)
          if (!loadingMore.value && cursor.value) {
            void prefetchNext()
          }
          continue
        }
        sentinelObserver?.unobserve(element)
        element.dataset.fired = 'true'
        if (!loadingMore.value && cursor.value) {
          void prefetchNext()
        }
      }
    },
    { rootMargin: SENTINEL_MARGIN },
  )
  return sentinelObserver
}

async function observeNewSentinels(): Promise<void> {
  await nextTick()
  const root = feedRoot.value
  if (!root) {
    return
  }
  const observer = ensureSentinelObserver()
  root.querySelectorAll<HTMLElement>('.feed-sentinel[data-feed-batch]:not([data-observed])').forEach((element) => {
    element.dataset.observed = 'true'
    observer.observe(element)
  })
}

// Fetch one page. The caller owns the loading flags; the generation check keeps
// a response that arrives after logout from mutating the next session's feed.
async function fetchBatch(reset: boolean, requestGeneration: number): Promise<boolean> {
  const page = await api.getFeed(
    reset ? null : cursor.value,
    reset ? 0 : pagesLoaded.value,
    FEED_LIMIT,
  )
  if (requestGeneration !== generation) {
    return false
  }

  if (reset) {
    items.value = page.items
    sentinelStarts.value = []
    pagesLoaded.value = 0
  } else if (page.items.length > 0) {
    const start = items.value.length
    items.value = [...items.value, ...page.items]
    if (page.next_cursor !== null) {
      sentinelStarts.value = [...sentinelStarts.value, start]
    }
  }

  cursor.value = page.next_cursor
  pagesLoaded.value += 1
  loaded.value = true
  return true
}

function armRetryOnNextScroll(): void {
  removeRetryScrollHandler()
  retryArmed = false
  retryScrollHandler = () => {
    retryScrollHandler = null
    if (!loadError.value || !cursor.value || loadingMore.value) {
      return
    }
    const sentinel = retrySentinel.value
    if (!sentinel) {
      return
    }
    retryArmed = true
    ensureSentinelObserver().observe(sentinel)
  }
  window.addEventListener('scroll', retryScrollHandler, { passive: true, once: true })
}

// Ads are interleaved server-side, so pagination keeps its own cursor and the
// feed simply appends the next page. Only one prefetch may be in flight.
async function prefetchNext(): Promise<void> {
  if (loadingMore.value || !cursor.value) {
    return
  }
  const requestGeneration = generation
  loadingMore.value = true
  loadError.value = false
  try {
    const ok = await fetchBatch(false, requestGeneration)
    if (ok) {
      await observeNewSentinels()
    }
  } catch {
    if (requestGeneration !== generation) {
      return
    }
    loadError.value = true
    await nextTick()
    armRetryOnNextScroll()
  } finally {
    loadingMore.value = false
  }
}

async function startFeed(): Promise<void> {
  generation += 1
  const requestGeneration = generation
  removeRetryScrollHandler()
  sentinelObserver?.disconnect()
  retryArmed = false
  items.value = []
  sentinelStarts.value = []
  cursor.value = null
  pagesLoaded.value = 0
  loaded.value = false
  loadError.value = false
  error.value = ''
  loading.value = true

  try {
    await fetchBatch(true, requestGeneration)
  } catch (err) {
    if (requestGeneration === generation) {
      error.value = errorMessage(err, 'Could not load your feed')
    }
    loading.value = false
    return
  }

  loading.value = false
  await nextTick()
  if (requestGeneration !== generation) {
    return
  }
  await observeNewSentinels()

  // Batch 1 is on screen now; eagerly warm batch 2 without waiting for scroll.
  if (cursor.value) {
    void prefetchNext()
  }
}

function resetFeed(): void {
  generation += 1
  removeRetryScrollHandler()
  sentinelObserver?.disconnect()
  retryArmed = false
  items.value = []
  sentinelStarts.value = []
  cursor.value = null
  pagesLoaded.value = 0
  loading.value = false
  loadingMore.value = false
  loaded.value = false
  error.value = ''
  loadError.value = false
}

onMounted(() => {
  if (isAuthenticated.value) {
    void startFeed()
  }
})

watch(isAuthenticated, (authenticated) => {
  if (authenticated) {
    void startFeed()
  } else {
    resetFeed()
  }
})

onBeforeUnmount(() => {
  removeRetryScrollHandler()
  sentinelObserver?.disconnect()
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
      <div ref="feedRoot" class="feed-stream" :aria-busy="loadingMore">
        <PostList
          :items="items"
          :sentinel-indexes="sentinelStarts"
          :empty-text="loaded ? 'Your feed is empty. Open a profile and follow someone to fill it.' : 'No barios yet.'"
        />
        <div
          v-if="loadError && cursor"
          ref="retrySentinel"
          class="feed-sentinel feed-retry-sentinel"
          data-retry="true"
          aria-hidden="true"
        ></div>
        <p v-if="loadError && cursor" class="feed-hint" role="status" aria-live="polite">
          Having trouble loading more…
        </p>
      </div>
    </template>
  </template>
</template>
