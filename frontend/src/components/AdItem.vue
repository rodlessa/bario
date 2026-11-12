<script setup lang="ts">
import { ref } from 'vue'
import { api, type FeedAdItem } from '../api'

const props = defineProps<{ ad: FeedAdItem }>()

const clicked = ref(false)

function recordClick(): void {
  if (clicked.value) {
    return
  }
  clicked.value = true
  void api.clickAd(props.ad.id).catch(() => {
    clicked.value = false
  })
}
</script>

<template>
  <article
    class="post ad"
    role="button"
    tabindex="0"
    @click="recordClick"
    @keydown.enter.prevent="recordClick"
    @keydown.space.prevent="recordClick"
  >
    <div class="post-meta">
      <span class="ad-label">Sponsored</span>
    </div>
    <h3 class="ad-title">{{ ad.title }}</h3>
    <p class="ad-body">{{ ad.body }}</p>
  </article>
</template>
