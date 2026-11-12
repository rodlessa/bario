<script setup lang="ts">
import { ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { useAuth } from '../auth'
import { errorMessage } from '../utils'

const { register } = useAuth()
const router = useRouter()

const username = ref('')
const email = ref('')
const password = ref('')
const confirmPassword = ref('')
const submitting = ref(false)
const error = ref('')

async function submit(): Promise<void> {
  if (submitting.value) {
    return
  }
  error.value = ''
  if (password.value !== confirmPassword.value) {
    error.value = 'Passwords do not match'
    return
  }

  submitting.value = true
  try {
    await register(username.value.trim(), email.value.trim(), password.value)
    await router.push('/')
  } catch (err) {
    error.value = errorMessage(err, 'Could not create your account')
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="narrow">
    <header class="page-head">
      <div>
        <h1>Create account</h1>
        <p class="muted">Three fields and you are chirping.</p>
      </div>
    </header>

    <form class="panel form" @submit.prevent="submit">
      <label class="field">
        <span>Username</span>
        <input
          v-model="username"
          name="username"
          autocomplete="username"
          minlength="3"
          maxlength="32"
          pattern="[A-Za-z0-9_]{3,32}"
          title="3-32 letters, digits or underscores"
          required
        />
        <small class="muted">3-32 letters, digits or underscores.</small>
      </label>

      <label class="field">
        <span>Email</span>
        <input v-model="email" name="email" type="email" autocomplete="email" required />
      </label>

      <label class="field">
        <span>Password</span>
        <input
          v-model="password"
          name="password"
          type="password"
          autocomplete="new-password"
          minlength="8"
          maxlength="72"
          required
        />
        <small class="muted">At least 8 characters.</small>
      </label>

      <label class="field">
        <span>Confirm password</span>
        <input
          v-model="confirmPassword"
          name="confirm-password"
          type="password"
          autocomplete="new-password"
          minlength="8"
          maxlength="72"
          required
        />
      </label>

      <p v-if="error" class="error" role="alert">{{ error }}</p>

      <button type="submit" class="button primary block" :disabled="submitting">
        {{ submitting ? 'Creating account…' : 'Create account' }}
      </button>

      <p class="muted form-foot">
        Already registered? <RouterLink class="text-link" to="/login">Log in</RouterLink>
      </p>
    </form>
  </section>
</template>
