export interface User {
  id: number
  username: string
  created_at?: string
}

export interface Author {
  id: number
  username: string
}

export interface UserRef {
  id: number
  username: string
}

export interface Post {
  id: number
  user_id: number
  content: string
  image_id: string | null
  created_at: string
  author: Author
}

export interface PostList {
  posts: Post[]
  next_cursor: string | null
}

export interface LoginResponse {
  token: string
  user: User
}

export interface UploadResponse {
  id: string
  url: string
}

const TOKEN_KEY = 'chirp.token'

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setToken(token: string | null): void {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token)
  } else {
    localStorage.removeItem(TOKEN_KEY)
  }
}

export class ApiError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body && !(init.body instanceof FormData)) {
    headers.set('Content-Type', 'application/json')
  }
  const token = getToken()
  if (token) {
    headers.set('Authorization', `Bearer ${token}`)
  }

  let response: Response
  try {
    response = await fetch(path, { ...init, headers })
  } catch {
    throw new ApiError('Could not reach the Chirp server', 0)
  }

  if (!response.ok) {
    let message = `Request failed (${response.status})`
    try {
      const body = (await response.json()) as { error?: string }
      if (body.error) {
        message = body.error
      }
    } catch {
      // Non-JSON error body, keep the status-based message.
    }
    throw new ApiError(message, response.status)
  }

  if (response.status === 204) {
    return undefined as T
  }
  const contentType = response.headers.get('Content-Type') ?? ''
  if (!contentType.includes('application/json')) {
    return undefined as T
  }
  return (await response.json()) as T
}

function cursorQuery(cursor?: string | null): string {
  return cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''
}

export const api = {
  register(username: string, email: string, password: string): Promise<{ id: number; username: string }> {
    return request('/api/auth/register', {
      method: 'POST',
      body: JSON.stringify({ username, email, password }),
    })
  },

  login(username: string, password: string): Promise<LoginResponse> {
    return request('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    })
  },

  getUser(id: number): Promise<User> {
    return request(`/api/auth/users/${id}`)
  },

  getUserPosts(id: number, cursor?: string | null): Promise<PostList> {
    return request(`/api/posts/users/${id}/posts${cursorQuery(cursor)}`)
  },

  getFeed(cursor?: string | null): Promise<PostList> {
    return request(`/api/posts/feed${cursorQuery(cursor)}`)
  },

  getPost(id: number): Promise<Post> {
    return request(`/api/posts/${id}`)
  },

  createPost(content: string, imageId?: string): Promise<Post> {
    return request('/api/posts', {
      method: 'POST',
      body: JSON.stringify({ content, image_id: imageId ?? null }),
    })
  },

  follow(id: number): Promise<{ following: boolean }> {
    return request(`/api/follows/users/${id}/follow`, { method: 'POST' })
  },

  unfollow(id: number): Promise<void> {
    return request(`/api/follows/users/${id}/follow`, { method: 'DELETE' })
  },

  getFollowers(id: number): Promise<UserRef[]> {
    return request(`/api/follows/users/${id}/followers`)
  },

  getFollowing(id: number): Promise<UserRef[]> {
    return request(`/api/follows/users/${id}/following`)
  },

  uploadImage(file: File): Promise<UploadResponse> {
    const body = new FormData()
    body.append('file', file)
    return request('/api/uploads', { method: 'POST', body })
  },
}
