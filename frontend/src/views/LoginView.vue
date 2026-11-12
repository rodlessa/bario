<script setup lang="ts">
import { ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { useAuth } from '../auth'
import { errorMessage } from '../utils'

const { login } = useAuth()
const route = useRoute()
const router = useRouter()

const username = ref('')
const password = ref('')
const submitting = ref(false)
const error = ref('')

async function submit(): Promise<void> {
  if (submitting.value) {
    return
  }
  submitting.value = true
  error.value = ''
  try {
    await login(username.value.trim(), password.value)
    const requested = route.query.redirect
    const destination = typeof requested === 'string' && requested.startsWith('/') ? requested : '/'
    await router.push(destination)
  } catch (err) {
    error.value = errorMessage(err, 'Could not log in')
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="narrow">
    <header class="page-head">
      <div>
        <h1>Log in</h1>
        <p class="muted">Pick up where you left off.</p>
      </div>
    </header>

    <form class="panel form" @submit.prevent="submit">
      <label class="field">
        <span>Username</span>
        <input v-model="username" name="username" autocomplete="username" required />
      </label>

      <label class="field">
        <span>Password</span>
        <input v-model="password" name="password" type="password" autocomplete="current-password" required />
      </label>

      <p v-if="error" class="error" role="alert">{{ error }}</p>

      <button type="submit" class="button primary block" :disabled="submitting">
        {{ submitting ? 'Logging in…' : 'Log in' }}
      </button>

      <p class="muted form-foot">
        New here? <RouterLink class="text-link" to="/register">Create an account</RouterLink>
      </p>
    </form>
  </section>
</template>
