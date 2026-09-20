const isProd = import.meta.env.PROD
const rawApiUrl = import.meta.env.VITE_API_URL
export const API_URL = (rawApiUrl && rawApiUrl !== 'http://localhost:4000')
  ? rawApiUrl
  : (isProd ? '/api' : 'http://127.0.0.1:4000')

export async function apiFetch<T = any>(endpoint: string, options: RequestInit & { skipAuth?: boolean } = {}): Promise<T> {
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
    if (response.status === 401) {
      localStorage.removeItem('pandrive.accessToken')
      localStorage.removeItem('pandrive.refreshToken')
      localStorage.removeItem('pandrive.user')
      window.location.href = '/login'
    }
    const errorData = await response.json().catch(() => ({}))
    throw new Error(errorData.message || `API error: ${response.status}`)
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

