<script setup lang="ts">
import { RouterLink } from 'vue-router'
import type { Post } from '../api'
import { formatRelativeTime } from '../utils'

defineProps<{ post: Post }>()
</script>

<template>
  <article class="post">
    <div class="post-meta">
      <RouterLink class="post-author" :to="`/u/${post.user_id}`">@{{ post.author.username }}</RouterLink>
      <span class="dot" aria-hidden="true">·</span>
      <RouterLink class="post-time" :to="`/post/${post.id}`">
        {{ formatRelativeTime(post.created_at) }}
      </RouterLink>
    </div>

    <RouterLink class="post-content" :to="`/post/${post.id}`">{{ post.content }}</RouterLink>

    <div v-if="post.image_id" class="post-image">
      <img
        :src="`/api/uploads/${post.image_id}`"
        :alt="`Image attached to the chirp by ${post.author.username}`"
        loading="lazy"
      />
    </div>
  </article>
</template>
