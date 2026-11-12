<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { api, type Post, type User } from '../api'
import { useAuth } from '../auth'
import PostList from '../components/PostList.vue'
import { errorMessage, formatDate } from '../utils'

const route = useRoute()
const router = useRouter()
const { state, isAuthenticated } = useAuth()

const userId = computed(() => Number(route.params.id))
const isSelf = computed(() => state.user !== null && state.user.id === userId.value)

const profile = ref<User | null>(null)
const posts = ref<Post[]>([])
const cursor = ref<string | null>(null)
const followerCount = ref(0)
const followingCount = ref(0)
const following = ref(false)
const loading = ref(true)
const loadingMore = ref(false)
const error = ref('')
const actionError = ref('')

async function loadProfile(): Promise<void> {
  if (!Number.isInteger(userId.value) || userId.value <= 0) {
    error.value = 'Invalid profile'
    loading.value = false
    return
  }

  loading.value = true
  error.value = ''
  actionError.value = ''
  try {
    const [user, page, followers, followingUsers] = await Promise.all([
      api.getUser(userId.value),
      api.getUserPosts(userId.value),
      api.getFollowers(userId.value),
      api.getFollowing(userId.value),
    ])
    profile.value = user
    posts.value = page.posts
    cursor.value = page.next_cursor
    followerCount.value = followers.length
    followingCount.value = followingUsers.length
    following.value = state.user !== null && followers.some((ref) => ref.id === state.user?.id)
  } catch (err) {
    error.value = errorMessage(err, 'Could not load this profile')
  } finally {
    loading.value = false
  }
}

async function loadMore(): Promise<void> {
  if (!cursor.value || loadingMore.value) {
    return
  }
  loadingMore.value = true
  try {
    const page = await api.getUserPosts(userId.value, cursor.value)
    posts.value = [...posts.value, ...page.posts]
    cursor.value = page.next_cursor
  } catch (err) {
    actionError.value = errorMessage(err, 'Could not load more chirps')
  } finally {
    loadingMore.value = false
  }
}

async function toggleFollow(): Promise<void> {
  if (!profile.value || isSelf.value) {
    return
  }
  if (!isAuthenticated.value) {
    await router.push({ name: 'login', query: { redirect: route.fullPath } })
    return
  }
  actionError.value = ''
  try {
    if (following.value) {
      await api.unfollow(profile.value.id)
      following.value = false
      followerCount.value = Math.max(0, followerCount.value - 1)
    } else {
      await api.follow(profile.value.id)
      following.value = true
      followerCount.value += 1
    }
  } catch (err) {
    actionError.value = errorMessage(err, 'Could not update follow state')
  }
}

onMounted(() => {
  void loadProfile()
})

watch(userId, () => {
  void loadProfile()
})
</script>

<template>
  <section v-if="loading" class="panel muted">Loading profile…</section>

  <section v-else-if="error" class="panel error" role="alert">{{ error }}</section>

  <template v-else-if="profile">
    <header class="panel profile-head">
      <div class="avatar" aria-hidden="true">{{ profile.username.slice(0, 1).toUpperCase() }}</div>
      <div class="profile-info">
        <h1>@{{ profile.username }}</h1>
        <p class="muted">Joined {{ formatDate(profile.created_at ?? '') }}</p>
        <p class="profile-stats">
          <strong>{{ followerCount }}</strong> followers
          <span class="dot" aria-hidden="true">·</span>
          <strong>{{ followingCount }}</strong> following
        </p>
      </div>
      <div class="profile-actions">
        <RouterLink v-if="isSelf" class="button primary" to="/compose">New chirp</RouterLink>
        <RouterLink
          v-else-if="!isAuthenticated"
          class="button primary"
          :to="{ name: 'login', query: { redirect: route.fullPath } }"
        >
          Log in to follow
        </RouterLink>
        <button v-else type="button" class="button" :class="following ? 'ghost' : 'primary'" @click="toggleFollow">
          {{ following ? 'Unfollow' : 'Follow' }}
        </button>
      </div>
    </header>

    <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>

    <h2 class="section-title">Chirps</h2>
    <PostList :posts="posts" empty-text="This profile has not chirped yet." />

    <div v-if="cursor" class="load-more">
      <button type="button" class="button" :disabled="loadingMore" @click="loadMore">
        {{ loadingMore ? 'Loading…' : 'Load more' }}
      </button>
    </div>
  </template>
</template>
