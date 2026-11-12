<script setup lang="ts">
import type { FeedItem } from '../api'
import AdItem from './AdItem.vue'
import PostItem from './PostItem.vue'

defineProps<{
  items: FeedItem[]
  emptyText?: string
}>()
</script>

<template>
  <div v-if="items.length > 0" class="post-list">
    <template v-for="item in items" :key="`${item.kind}-${item.id}`">
      <AdItem v-if="item.kind === 'ad'" :ad="item" />
      <PostItem v-else :post="item" />
    </template>
  </div>
  <p v-else class="empty">{{ emptyText ?? 'No barios yet.' }}</p>
</template>
