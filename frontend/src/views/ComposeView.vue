<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { errorMessage } from '../utils'

const MAX_CONTENT = 280
const MAX_IMAGE_BYTES = 10 * 1024 * 1024

const router = useRouter()

const content = ref('')
const file = ref<File | null>(null)
const previewUrl = ref<string | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
const submitting = ref(false)
const error = ref('')

const remaining = computed(() => MAX_CONTENT - content.value.length)
const canSubmit = computed(() => content.value.trim().length > 0 && remaining.value >= 0 && !submitting.value)

function clearFile(): void {
  if (previewUrl.value) {
    URL.revokeObjectURL(previewUrl.value)
  }
  previewUrl.value = null
  file.value = null
  if (fileInput.value) {
    fileInput.value.value = ''
  }
}

function onFileChange(event: Event): void {
  const input = event.target as HTMLInputElement
  const selected = input.files?.[0] ?? null
  error.value = ''
  if (!selected) {
    clearFile()
    return
  }
  if (!selected.type.startsWith('image/')) {
    error.value = 'Please choose an image file.'
    clearFile()
    return
  }
  if (selected.size > MAX_IMAGE_BYTES) {
    error.value = 'Images must be 10 MiB or smaller.'
    clearFile()
    return
  }
  clearFile()
  file.value = selected
  previewUrl.value = URL.createObjectURL(selected)
}

async function submit(): Promise<void> {
  if (!canSubmit.value) {
    return
  }
  submitting.value = true
  error.value = ''
  try {
    let imageId: string | undefined
    if (file.value) {
      const uploaded = await api.uploadImage(file.value)
      imageId = uploaded.id
    }
    const created = await api.createPost(content.value.trim(), imageId)
    clearFile()
    await router.push(`/post/${created.id}`)
  } catch (err) {
    error.value = errorMessage(err, 'Could not publish your chirp')
  } finally {
    submitting.value = false
  }
}

onBeforeUnmount(clearFile)
</script>

<template>
  <section class="page-head">
    <div>
      <h1>New chirp</h1>
      <p class="muted">Say something short. Attach an image if you like.</p>
    </div>
  </section>

  <form class="panel compose" @submit.prevent="submit">
    <textarea
      v-model="content"
      class="compose-text"
      name="content"
      rows="5"
      placeholder="What is happening?"
      aria-label="Chirp content"
    ></textarea>

    <div class="compose-toolbar">
      <label class="button ghost file-button">
        {{ file ? 'Change image' : 'Add image' }}
        <input ref="fileInput" type="file" accept="image/*" hidden @change="onFileChange" />
      </label>
      <span class="counter" :class="{ over: remaining < 0 }">{{ remaining }}</span>
    </div>

    <div v-if="previewUrl" class="compose-preview">
      <img :src="previewUrl" alt="Selected image preview" />
      <button type="button" class="button ghost" @click="clearFile">Remove image</button>
    </div>

    <p v-if="error" class="error" role="alert">{{ error }}</p>

    <div class="compose-actions">
      <button type="submit" class="button primary" :disabled="!canSubmit">
        {{ submitting ? 'Publishing…' : 'Publish chirp' }}
      </button>
    </div>
  </form>
</template>
