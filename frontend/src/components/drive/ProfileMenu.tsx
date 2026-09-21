import { type FormEvent, useEffect, useRef, useState } from 'react'
import { ChevronDown, KeyRound, Languages, LogOut, Settings, UserRound } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { DummyModal } from '@/components/drive/DummyModal'
import { Input } from '@/components/ui/input'
import { apiFetch } from '@/lib/api'
import { setAuthSession, type AuthUser } from '@/lib/auth'
import { cn } from '@/lib/utils'
import { useI18n } from '@/lib/i18n'

// The profile lives in the header next to the bell so it is reachable from every page:
// edit name/email, change the password (the old one is required by the server), or log out.
export function ProfileMenu({ user, onUserChange, onLogout, className }: {
  user: AuthUser | null
  onUserChange: (user: AuthUser) => void
  onLogout: () => void
  className?: string
}) {
  const { t, lang, setLang } = useI18n()
  const [open, setOpen] = useState(false)
  const [profileOpen, setProfileOpen] = useState(false)
  const [passwordOpen, setPasswordOpen] = useState(false)
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')
  const [error, setError] = useState('')
  const containerRef = useRef<HTMLDivElement | null>(null)

  const initial = (user?.name ?? user?.email ?? 'U').trim().charAt(0).toUpperCase()

  useEffect(() => {
    if (!open) return
    function onDocumentClick(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDocumentClick)
    return () => document.removeEventListener('mousedown', onDocumentClick)
  }, [open])

  function openProfile() {
    setName(user?.name ?? '')
    setEmail(user?.email ?? '')
    setMessage('')
    setError('')
    setOpen(false)
    setProfileOpen(true)
  }

  function openPassword() {
    setCurrentPassword('')
    setNewPassword('')
    setConfirmPassword('')
    setMessage('')
    setError('')
    setOpen(false)
    setPasswordOpen(true)
  }

  async function saveProfile(event: FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      const result = await apiFetch<{ accessToken: string; refreshToken: string; user: AuthUser }>('/auth/me', {
        method: 'PUT',
        body: JSON.stringify({ name, email }),
      })
      setAuthSession(result.accessToken, result.refreshToken, result.user)
      onUserChange(result.user)
      setProfileOpen(false)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save profile')
    } finally {
      setBusy(false)
    }
  }

  async function savePassword(event: FormEvent) {
    event.preventDefault()
    if (newPassword !== confirmPassword) {
      setError('The new password and confirmation do not match.')
      return
    }
    setBusy(true)
    setError('')
    try {
      const result = await apiFetch<{ accessToken: string; refreshToken: string; user: AuthUser }>('/auth/change-password', {
        method: 'POST',
        body: JSON.stringify({ currentPassword, newPassword }),
      })
      setAuthSession(result.accessToken, result.refreshToken, result.user)
      onUserChange(result.user)
      setPasswordOpen(false)
      setMessage('Password changed. Other devices have been signed out.')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to change password')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className={cn('relative', className)} ref={containerRef}>
      <Button
        variant="outline"
        size="sm"
        className="h-10 gap-2 px-2 pr-2.5"
        aria-label="Open profile menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-blue-500 to-indigo-600 text-[11px] font-bold text-white">
          {initial}
        </span>
        <span className="hidden max-w-[9rem] truncate text-[13px] font-bold xl:inline">{user?.name ?? 'Profile'}</span>
        <ChevronDown className={cn('h-3.5 w-3.5 transition-transform', open && 'rotate-180')} />
      </Button>

      {open ? (
        <div className="absolute right-0 top-12 z-[60] w-[min(calc(100vw-2rem),17rem)] overflow-hidden rounded-2xl border border-slate-200 bg-white shadow-2xl shadow-slate-950/15">
          <div className="flex items-center gap-2.5 border-b border-slate-100 bg-slate-50/60 px-3.5 py-3">
            <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-blue-500 to-indigo-600 text-sm font-bold text-white">
              {initial}
            </span>
            <div className="min-w-0">
              <p className="truncate text-[13px] font-bold text-slate-900">{user?.name ?? 'User'}</p>
              <p className="truncate text-[11px] text-slate-500">{user?.email ?? ''}</p>
            </div>
          </div>
          <div className="p-1.5">
            {message ? <p className="mx-1 mb-1 rounded-lg bg-emerald-50 px-2.5 py-2 text-[11px] font-semibold text-emerald-700">{message}</p> : null}
            <MenuItem icon={UserRound} label={t('Edit profile')} onClick={openProfile} />
            <MenuItem icon={KeyRound} label={t('Change password')} onClick={openPassword} />
            <MenuItem icon={Settings} label={t('Settings')} onClick={() => { setOpen(false); window.location.href = '/settings' }} />
            <MenuItem icon={Languages} label={lang === 'id' ? 'Bahasa: Indonesia' : 'Language: English'} onClick={() => setLang(lang === 'en' ? 'id' : 'en')} />
            <div className="my-1 h-px bg-slate-100" />
            <MenuItem icon={LogOut} label={t('Log out')} onClick={() => { setOpen(false); onLogout() }} danger />
          </div>
        </div>
      ) : null}

      <DummyModal open={profileOpen} title="Edit profile" description="Change the name and email shown across PanDrive." onClose={() => setProfileOpen(false)}>
        <form onSubmit={saveProfile} className="grid gap-4">
          <label className="grid gap-2 text-sm font-semibold">Name<Input value={name} onChange={(e) => setName(e.target.value)} required /></label>
          <label className="grid gap-2 text-sm font-semibold">Email<Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required /></label>
          {error ? <p className="rounded-xl bg-red-50 p-3 text-sm font-semibold text-red-700">{error}</p> : null}
          <div className="flex justify-end gap-3 pt-1">
            <Button type="button" variant="outline" onClick={() => setProfileOpen(false)}>Cancel</Button>
            <Button disabled={busy}>{busy ? 'Saving...' : 'Save profile'}</Button>
          </div>
        </form>
      </DummyModal>

      <DummyModal open={passwordOpen} title="Change password" description="Your current password is required and every other device is signed out." onClose={() => setPasswordOpen(false)}>
        <form onSubmit={savePassword} className="grid gap-4">
          <label className="grid gap-2 text-sm font-semibold">Current password<Input type="password" autoComplete="current-password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} required /></label>
          <label className="grid gap-2 text-sm font-semibold">New password<Input type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} required minLength={8} /></label>
          <label className="grid gap-2 text-sm font-semibold">Confirm new password<Input type="password" autoComplete="new-password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} required minLength={8} /></label>
          <p className="text-[11px] text-slate-500">Minimum 8 characters.</p>
          {error ? <p className="rounded-xl bg-red-50 p-3 text-sm font-semibold text-red-700">{error}</p> : null}
          <div className="flex justify-end gap-3 pt-1">
            <Button type="button" variant="outline" onClick={() => setPasswordOpen(false)}>Cancel</Button>
            <Button disabled={busy}>{busy ? 'Updating...' : 'Change password'}</Button>
          </div>
        </form>
      </DummyModal>
    </div>
  )
}

function MenuItem({ icon: Icon, label, onClick, danger = false }: { icon: React.ElementType; label: string; onClick: () => void; danger?: boolean }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-[13px] font-semibold transition-colors',
        danger ? 'text-red-600 hover:bg-red-50' : 'text-slate-700 hover:bg-slate-100',
      )}
    >
      <Icon className="h-4 w-4" />
      {label}
    </button>
  )
}
