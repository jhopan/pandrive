import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { apiFetch } from '@/lib/api'
import { setAuthSession, type AuthUser } from '@/lib/auth'
import { useI18n } from '@/lib/i18n'

type AuthResponse = { accessToken: string; refreshToken: string; user: AuthUser }

export function LoginPage() {
  const navigate = useNavigate()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const { t, lang, setLang } = useI18n()

  async function submit(event: FormEvent) {
    event.preventDefault()
    setLoading(true)
    setError('')
    try {
      const data = await apiFetch<AuthResponse>('/auth/login', { method: 'POST', skipAuth: true, body: JSON.stringify({ email, password }) })
      setAuthSession(data.accessToken, data.refreshToken, data.user)
      navigate('/all-files')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Login failed')
    } finally {
      setLoading(false)
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50 p-5">
      <Card className="w-full max-w-md p-6">
        <div className="flex items-center gap-3">
          <img src="/logo.png" alt="PanDrive" width={44} height={44} className="h-11 w-11 rounded-xl object-cover shadow-lg shadow-blue-500/20" />
          <div className="min-w-0 flex-1"><h1 className="text-2xl font-extrabold">{t('Login')}</h1><p className="text-sm text-slate-500">{t('Access your PanDrive gateway.')}</p></div>
          <button type="button" onClick={() => setLang(lang === 'en' ? 'id' : 'en')} className="shrink-0 rounded-lg border border-slate-200 px-2 py-1 text-[11px] font-extrabold text-slate-500 hover:bg-slate-100" title="Bahasa / Language">{lang.toUpperCase()}</button>
        </div>
        <form onSubmit={submit} className="mt-6 grid gap-4" autoComplete="off">
          <label className="grid gap-2 text-sm font-semibold">{t('Email')}<Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="off" /></label>
          <label className="grid gap-2 text-sm font-semibold">{t('Password')}<Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="new-password" /></label>
          {error ? <p className="rounded-xl bg-red-50 p-3 text-sm text-red-600">{error}</p> : null}
          <Button disabled={loading}>{loading ? (lang === 'id' ? 'Masuk...' : 'Logging in...') : t('Login')}</Button>
        </form>
        <p className="mt-6 text-center text-xs text-slate-400">
          PanDrive by <a href="https://github.com/jhopan" target="_blank" rel="noopener noreferrer" className="font-semibold hover:underline">JhopanStore</a>
        </p>
      </Card>
    </main>
  )
}
