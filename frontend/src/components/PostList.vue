<script setup lang="ts">
import { computed } from 'vue'
import type { FeedItem } from '../api'
import AdItem from './AdItem.vue'
import PostItem from './PostItem.vue'

const props = defineProps<{
  items: FeedItem[]
  emptyText?: string
  sentinelIndexes?: number[]
}>()

const sentinelStarts = computed(() => new Set(props.sentinelIndexes ?? []))
</script>

<template>
  <div v-if="items.length > 0" class="post-list">
    <template v-for="(item, index) in items" :key="`${item.kind}-${item.id}`">
      <div
        v-if="sentinelStarts.has(index)"
        class="feed-sentinel"
        data-feed-batch="true"
        aria-hidden="true"
      ></div>
      <AdItem v-if="item.kind === 'ad'" :ad="item" />
      <PostItem v-else :post="item" />
    </template>
  </div>
  <p v-else class="empty">{{ emptyText ?? 'No barios yet.' }}</p>
</template>
