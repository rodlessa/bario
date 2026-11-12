<script setup lang="ts">
import type { Post } from '../api'
import PostItem from './PostItem.vue'

defineProps<{
  posts: Post[]
  emptyText?: string
}>()
</script>

<template>
  <div v-if="posts.length > 0" class="post-list">
    <PostItem v-for="post in posts" :key="post.id" :post="post" />
  </div>
  <p v-else class="empty">{{ emptyText ?? 'No chirps yet.' }}</p>
</template>
