<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { api, type Post } from '../api'
import PostItem from '../components/PostItem.vue'
import { errorMessage } from '../utils'

const route = useRoute()
const postId = computed(() => Number(route.params.id))

const post = ref<Post | null>(null)
const loading = ref(true)
const error = ref('')

async function loadPost(): Promise<void> {
  if (!Number.isInteger(postId.value) || postId.value <= 0) {
    error.value = 'Invalid post'
    loading.value = false
    return
  }
  loading.value = true
  error.value = ''
  try {
    post.value = await api.getPost(postId.value)
  } catch (err) {
    error.value = errorMessage(err, 'Could not load this chirp')
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void loadPost()
})

watch(postId, () => {
  void loadPost()
})
</script>

<template>
  <section class="page-head">
    <div>
      <h1>Chirp</h1>
      <RouterLink class="text-link" to="/">← Back to the feed</RouterLink>
    </div>
  </section>

  <p v-if="loading" class="panel muted">Loading chirp…</p>
  <p v-else-if="error" class="panel error" role="alert">{{ error }}</p>

  <template v-else-if="post">
    <PostItem :post="post" />
    <p class="muted thread-note">
      View more from
      <RouterLink class="text-link" :to="`/u/${post.user_id}`">@{{ post.author.username }}</RouterLink>.
    </p>
  </template>
</template>
