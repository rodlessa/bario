import { computed, reactive } from 'vue'
import { api, getToken, setToken, type User } from './api'

const USER_KEY = 'bario.user'

interface AuthState {
  token: string | null
  user: User | null
}

function readStoredUser(): User | null {
  const raw = localStorage.getItem(USER_KEY)
  if (!raw) {
    return null
  }
  try {
    const parsed = JSON.parse(raw) as User
    if (typeof parsed.id === 'number' && typeof parsed.username === 'string') {
      return parsed
    }
  } catch {
    // Corrupt value: treat as signed out.
  }
  localStorage.removeItem(USER_KEY)
  return null
}

const state = reactive<AuthState>({
  token: getToken(),
  user: readStoredUser(),
})

function persist(token: string | null, user: User | null): void {
  state.token = token
  state.user = user
  setToken(token)
  if (user) {
    localStorage.setItem(USER_KEY, JSON.stringify(user))
  } else {
    localStorage.removeItem(USER_KEY)
  }
}

async function login(username: string, password: string): Promise<void> {
  const response = await api.login(username, password)
  persist(response.token, response.user)
}

async function register(username: string, email: string, password: string): Promise<void> {
  await api.register(username, email, password)
  await login(username, password)
}

function logout(): void {
  persist(null, null)
}

export function useAuth() {
  return {
    state,
    isAuthenticated: computed(() => state.user !== null),
    login,
    register,
    logout,
  }
}
