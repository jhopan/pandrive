const isProd = import.meta.env.PROD
const rawApiUrl = import.meta.env.VITE_API_URL
export const API_URL = (rawApiUrl && rawApiUrl !== 'http://localhost:4000')
  ? rawApiUrl
  : (isProd ? '/api' : 'http://127.0.0.1:4000')

let refreshInFlight: Promise<boolean> | null = null

// refreshSession exchanges the stored refresh token for a new pair (rotation-aware).
async function refreshSession(): Promise<boolean> {
  if (refreshInFlight) return refreshInFlight
  const refreshToken = localStorage.getItem('pandrive.refreshToken')
  if (!refreshToken) return false
  refreshInFlight = (async () => {
    try {
      const res = await fetch(`${API_URL}/auth/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refreshToken })
      })
      if (!res.ok) return false
      const data = await res.json() as { accessToken: string; refreshToken?: string; user?: unknown }
      localStorage.setItem('pandrive.accessToken', data.accessToken)
      // Rotation: server revokes the old refresh token and returns a new one.
      if (data.refreshToken) localStorage.setItem('pandrive.refreshToken', data.refreshToken)
      if (data.user) localStorage.setItem('pandrive.user', JSON.stringify(data.user))
      return true
    } catch {
      return false
    } finally {
      refreshInFlight = null
    }
  })()
  return refreshInFlight
}

function clearSessionAndLogin() {
  localStorage.removeItem('pandrive.accessToken')
  localStorage.removeItem('pandrive.refreshToken')
  localStorage.removeItem('pandrive.user')
  window.location.href = '/login'
}

export async function apiFetch<T = any>(endpoint: string, options: RequestInit & { skipAuth?: boolean } = {}, retried = false): Promise<T> {
  const token = localStorage.getItem('pandrive.accessToken')
  const headers = new Headers(options.headers || {})
  
  if (token && !options.skipAuth) {
    headers.set('Authorization', `Bearer ${token}`)
  }
  if (!headers.has('Content-Type') && !(options.body instanceof FormData)) {
    headers.set('Content-Type', 'application/json')
  }

  // Remove custom property before passing to fetch
  const fetchOptions = { ...options }
  delete fetchOptions.skipAuth

  const response = await fetch(`${API_URL}${endpoint}`, {
    ...fetchOptions,
    headers
  })

  if (!response.ok) {
    // Access token expired -> refresh once (with rotation) and replay the request.
    if (response.status === 401 && !options.skipAuth && !retried) {
      const ok = await refreshSession()
      if (ok) return apiFetch<T>(endpoint, options, true)
      clearSessionAndLogin()
      throw new Error('Session expired')
    }
    if (response.status === 401) {
      clearSessionAndLogin()
    }
    const errorData = await response.json().catch(() => ({}))
    const err = new Error(errorData.message || `API error: ${response.status}`) as Error & { code?: string; status?: number }
    err.code = errorData.code
    err.status = response.status
    throw err
  }

  return response.json()
}

export function formatBytes(bytes: number | string | null | undefined, decimals = 2) {
  if (bytes == null) return '0 Bytes'
  const b = typeof bytes === 'string' ? parseFloat(bytes) : bytes
  if (!+b) return '0 Bytes'
  const k = 1024
  const dm = decimals < 0 ? 0 : decimals
  const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB', 'ZB', 'YB']
  const i = Math.floor(Math.log(b) / Math.log(k))
  return `${parseFloat((b / Math.pow(k, i)).toFixed(dm))} ${sizes[i]}`
}

export function formatDate(dateString: string | undefined | null) {
  if (!dateString) return ''
  return new Date(dateString).toLocaleDateString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit'
  })
}

