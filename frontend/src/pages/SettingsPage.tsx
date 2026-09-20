import { useEffect, useState, useRef } from 'react'
import { Bell, Cloud, Database, Globe, HardDrive, Link2, RefreshCw, Trash2, Copy } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { DummyModal } from '@/components/drive/DummyModal'
import { OAuthConfigManager } from '@/components/drive/OAuthConfigManager'
import { PageHeader } from '@/components/drive/PageHeader'
import { apiFetch, formatBytes, API_URL } from '@/lib/api'
import { getGravatarUrl } from '@/lib/gravatar'
import { getStoredUser, getAccessToken, clearAuthSession } from '@/lib/auth'

type ConnectedAccount = { id: string; provider: string; email: string; displayName?: string | null; status: string; needsReconnect?: boolean; storageAccount?: { totalBytes: string | null; usedBytes: string; availableBytes: string | null; lastSyncedAt: string | null } | null }

function providerLabel(provider: string) {
  if (provider === 's3') return 'S3 Storage'
  return 'Google Drive'
}

function storageLimitLabel(account: ConnectedAccount) {
  if (account.provider === 's3' && account.storageAccount?.totalBytes === null) return 'Unlimited'
  return formatBytes(account.storageAccount?.totalBytes)
}

function availableLabel(account: ConnectedAccount) {
  if (account.provider === 's3' && account.storageAccount?.availableBytes === null) return 'Unlimited'
  return formatBytes(account.storageAccount?.availableBytes)
}

export function SettingsPage() {
  const user = getStoredUser()
  const [accounts, setAccounts] = useState<ConnectedAccount[]>([])
  const [message, setMessage] = useState('')
  const [connecting, setConnecting] = useState(false)
  const [syncingAccountId, setSyncingAccountId] = useState<string | null>(null)
  const [disconnectingAccountId, setDisconnectingAccountId] = useState<string | null>(null)
  const [accountToDisconnect, setAccountToDisconnect] = useState<ConnectedAccount | null>(null)
  const [profileImageUrl, setProfileImageUrl] = useState('')
  const [avatarError, setAvatarError] = useState(false)
  const [selectedAccountId, setSelectedAccountId] = useState('')
  const [updatingSystem, setUpdatingSystem] = useState(false)
  const [updateModalOpen, setUpdateModalOpen] = useState(false)
  const [updateModalTitle, setUpdateModalTitle] = useState('')

  // Google OAuth Config states
  const [googleConnectUrl, setGoogleConnectUrl] = useState('')
  const [showGoogleConnectModal, setShowGoogleConnectModal] = useState(false)

  // Live log polling states
  const [isPollingLog, setIsPollingLog] = useState(false)
  const [updateLog, setUpdateLog] = useState('')
  const [updateFinished, setUpdateFinished] = useState(false)
  const [updateSuccess, setUpdateSuccess] = useState<boolean | null>(null)
  const [reconnectCount, setReconnectCount] = useState(0)
  const logContainerRef = useRef<HTMLDivElement>(null)

  // Backup & Restore states
  const [downloadingBackup, setDownloadingBackup] = useState(false)
  const [restoringBackup, setRestoringBackup] = useState(false)
  const [restoreFile, setRestoreFile] = useState<File | null>(null)
  const [restoreMessage, setRestoreMessage] = useState('')
  const [restoreSuccess, setRestoreSuccess] = useState(false)

  async function downloadBackup() {
    setDownloadingBackup(true)
    try {
      const token = getAccessToken()
      const response = await fetch(`${API_URL}/system/backup`, {
        headers: {
          'Authorization': `Bearer ${token}`
        }
      })
      if (!response.ok) {
        throw new Error('Failed to retrieve database backup.')
      }
      const blob = await response.blob()
      const url = window.URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = 'pandrive-backup.db'
      document.body.appendChild(a)
      a.click()
      a.remove()
      window.URL.revokeObjectURL(url)
    } catch (err: any) {
      alert('Failed to download backup: ' + err.message)
    } finally {
      setDownloadingBackup(false)
    }
  }

  function handleRestoreFileChange(e: React.ChangeEvent<HTMLInputElement>) {
    if (e.target.files && e.target.files.length > 0) {
      setRestoreFile(e.target.files[0])
    } else {
      setRestoreFile(null)
    }
  }

  async function restoreBackup() {
    if (!restoreFile) return
    if (!confirm('WARNING: Restoring database will overwrite all your current configurations, connected accounts, virtual folders, and user accounts. The server will restart. Are you sure you want to proceed?')) {
      return
    }

    setRestoringBackup(true)
    setRestoreMessage('')
    setRestoreSuccess(false)

    try {
      const token = getAccessToken()
      const formData = new FormData()
      formData.append('file', restoreFile)

      const response = await fetch(`${API_URL}/system/restore`, {
        method: 'POST',
        headers: {
          'Authorization': `Bearer ${token}`
        },
        body: formData
      })

      const data = await response.json()
      if (!response.ok) {
        throw new Error(data.message || 'Failed to restore database.')
      }

      setRestoreSuccess(true)
      setRestoreMessage(data.message || 'Database restored successfully! Logging you out and reloading...')

      setTimeout(() => {
        clearAuthSession()
        window.location.href = '/login'
      }, 4000)

    } catch (err: any) {
      setRestoreSuccess(false)
      setRestoreMessage(err.message || 'Failed to restore database.')
    } finally {
      setRestoringBackup(false)
    }
  }

  useEffect(() => {
    if (!isPollingLog) return

    let intervalId: any
    let active = true

    async function fetchLog() {
      try {
        const data = await apiFetch<{ log: string }>('/system/update-log')
        if (!active) return

        setUpdateLog(data.log)
        setReconnectCount(0)

        if (data.log.includes('=== System Update Completed:')) {
          setUpdateFinished(true)
          setUpdateSuccess(true)
          setIsPollingLog(false)
          setUpdateModalTitle('System Updated')
        }
      } catch (err) {
        if (!active) return
        setReconnectCount((prev) => prev + 1)
      }
    }

    fetchLog()
    intervalId = setInterval(fetchLog, 2000)

    return () => {
      active = false
      clearInterval(intervalId)
    }
  }, [isPollingLog])

  useEffect(() => {
    if (logContainerRef.current) {
      logContainerRef.current.scrollTop = logContainerRef.current.scrollHeight
    }
  }, [updateLog])

  async function runSystemUpdate() {
    setUpdatingSystem(true)
    setMessage('')
    setUpdateLog('Initiating system update in the background...\n')
    setUpdateFinished(false)
    setUpdateSuccess(null)
    setReconnectCount(0)
    setUpdateModalTitle('System Updating')
    setUpdateModalOpen(true)

    try {
      await apiFetch<{ message: string }>('/system/update', { method: 'POST' })
      setIsPollingLog(true)
    } catch (error) {
      setUpdateModalTitle('System Update Failed')
      const errMsg = error instanceof Error ? error.message : 'System update failed to initiate.'
      setUpdateLog((prev) => prev + `\nError: ${errMsg}`)
      setUpdateFinished(true)
      setUpdateSuccess(false)
    } finally {
      setUpdatingSystem(false)
    }
  }


  const selectedAccount = accounts.find((account) => account.id === selectedAccountId) ?? accounts[0] ?? null

  async function load() {
    const data = await apiFetch<{ accounts: ConnectedAccount[] }>('/connected-accounts')
    setAccounts(data.accounts)
  }

  useEffect(() => {
    load().catch((error) => setMessage(error instanceof Error ? error.message : 'Failed to load settings'))
  }, [])

  useEffect(() => {
    setAvatarError(false)
    getGravatarUrl(user?.email, 96).then(setProfileImageUrl).catch(() => setProfileImageUrl(''))
  }, [user?.email])

  useEffect(() => {
    if (accounts.length === 0) {
      setSelectedAccountId('')
      return
    }
    if (!accounts.some((account) => account.id === selectedAccountId)) setSelectedAccountId(accounts[0].id)
  }, [accounts, selectedAccountId])

  useEffect(() => {
    function onMessage(event: MessageEvent) {
      if (event.origin !== window.location.origin || event.data?.type !== 'GOOGLE_CONNECTED') return
      setMessage(event.data.status === 'success' ? 'Google Drive connected.' : 'Google Drive connection failed.')
      load().then(() => {
        window.dispatchEvent(new Event('pandrive:storage-changed'))
      }).catch(() => undefined)
    }
    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
  }, [])

  const [googleCallbackUrl, setGoogleCallbackUrl] = useState('')
  const [googleCallbackLoading, setGoogleCallbackLoading] = useState(false)

  async function connectDrive() {
    setConnecting(true)
    setMessage('')
    setGoogleCallbackUrl('')
    try {
      const data = await apiFetch<{ url: string }>('/connected-accounts/google/connect-url')
      setGoogleConnectUrl(data.url)
      setShowGoogleConnectModal(true)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to generate Google Drive connection link')
    } finally {
      setConnecting(false)
    }
  }

  async function handleManualCallback() {
    if (!googleCallbackUrl) return
    setGoogleCallbackLoading(true)
    try {
      // Extract search params
      let searchParams: URLSearchParams
      try {
        searchParams = new URL(googleCallbackUrl).searchParams
      } catch (e) {
        throw new Error('Invalid URL format')
      }
      
      const code = searchParams.get('code')
      const state = searchParams.get('state')
      if (!code || !state) throw new Error('Missing code or state in the URL')

      await apiFetch(`/connected-accounts/google/callback?code=${code}&state=${state}`, {
        method: 'GET' // using API fetch allows manual callback processing without redirecting whole page
      })
      
      setMessage('Google Drive connected successfully!')
      setShowGoogleConnectModal(false)
      load()
      window.dispatchEvent(new Event('pandrive:storage-changed'))
    } catch (error) {
      alert(error instanceof Error ? error.message : 'Failed to connect account')
    } finally {
      setGoogleCallbackLoading(false)
    }
  }

  async function sync(accountId: string) {
    setSyncingAccountId(accountId)
    try {
      await apiFetch(`/connected-accounts/${accountId}/sync-quota`, { method: 'POST' })
      await load()
      window.dispatchEvent(new Event('pandrive:storage-changed'))
    } finally {
      setSyncingAccountId(null)
    }
  }

  async function disconnect() {
    if (!accountToDisconnect) return
    setDisconnectingAccountId(accountToDisconnect.id)
    setMessage('')
    try {
      await apiFetch(`/connected-accounts/${accountToDisconnect.id}`, { method: 'DELETE' })
      setAccountToDisconnect(null)
      setMessage('Storage account disconnected.')
      await load()
      window.dispatchEvent(new Event('pandrive:storage-changed'))
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Failed to disconnect Google Drive account')
    } finally {
      setDisconnectingAccountId(null)
    }
  }


  return (
    <>
      <DummyModal
        open={showGoogleConnectModal}
        onClose={() => {
          setShowGoogleConnectModal(false)
          load()
        }}
        title="Connect Google Drive"
        description="Authorize offline access manually via localhost callback"
        className="max-w-md"
      >
        <div className="grid gap-6 py-2">
          {/* STEP 1 */}
          <div className="grid gap-2">
            <h3 className="text-sm font-bold text-slate-800 dark:text-slate-200">Step 1: Open this URL in your browser</h3>
            <p className="text-[12px] text-slate-500">
              Open the link below, select your Google account, and grant access.
            </p>
            <div className="flex gap-2">
              <input
                type="text"
                readOnly
                value={googleConnectUrl}
                className="flex-1 rounded-xl border border-slate-200 bg-slate-50 px-3 py-2 text-sm text-slate-500 font-mono overflow-x-auto whitespace-nowrap"
              />
              <Button
                variant="outline"
                className="shrink-0"
                onClick={() => {
                  navigator.clipboard.writeText(googleConnectUrl)
                  setMessage('Authorization link copied to clipboard!')
                }}
              >
                <Copy className="mr-2 h-4 w-4" /> Copy
              </Button>
            </div>
            <div className="flex justify-end">
              <Button variant="ghost" size="sm" className="text-xs text-blue-600" onClick={() => window.open(googleConnectUrl, '_blank')}>
                Open in new tab
              </Button>
            </div>
          </div>

          <div className="relative flex items-center justify-center">
            <div className="absolute inset-0 flex items-center">
              <div className="w-full border-t border-slate-200 dark:border-slate-800"></div>
            </div>
            <div className="relative bg-white dark:bg-slate-950 px-4 text-[10px] font-bold uppercase tracking-wider text-slate-400">
              THEN
            </div>
          </div>

          {/* STEP 2 */}
          <div className="grid gap-2">
            <h3 className="text-sm font-bold text-slate-800 dark:text-slate-200">Step 2: Paste the callback URL here</h3>
            <p className="text-[12px] text-slate-500">
              After granting access, your browser will redirect to a <code>localhost</code> URL (which may show an error). Copy the <strong>full URL</strong> from your browser's address bar and paste it below.
            </p>
            <div className="flex gap-2 mt-1">
              <input
                type="text"
                placeholder="http://localhost:4000/...callback?state=...&code=..."
                value={googleCallbackUrl}
                onChange={(e) => setGoogleCallbackUrl(e.target.value)}
                className="flex-1 rounded-xl border border-slate-200 bg-white px-3 py-2 text-sm focus:border-blue-500 focus:outline-none"
              />
            </div>
            <div className="flex justify-end mt-2">
              <Button onClick={handleManualCallback} disabled={googleCallbackLoading || !googleCallbackUrl}>
                {googleCallbackLoading ? 'Connecting...' : 'Connect'}
              </Button>
            </div>
          </div>
        </div>
      </DummyModal>
      <PageHeader title="Setting" description="Manage account and connected storage." actions={<Button size="sm" onClick={connectDrive} disabled={connecting}><Link2 className="h-4 w-4" />{connecting ? 'Connecting...' : 'Connect Drive'}</Button>} />
      {message ? <p className="mt-4 rounded-xl bg-blue-50 p-3 text-sm text-blue-700">{message}</p> : null}
      <div className="mt-5 grid gap-4 lg:grid-cols-[1fr_280px]">
        <div className="grid gap-4">
          <Card className="p-4">
            <div className="flex items-center gap-3.5">
              {!profileImageUrl || avatarError ? (
                <div className="flex h-12 w-12 shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-blue-500 to-indigo-600 text-lg font-bold text-white shadow-sm border border-blue-400/20 sm:h-14 sm:w-14">
                  {(user?.name ?? user?.email ?? 'U').trim().charAt(0).toUpperCase()}
                </div>
              ) : (
                <img
                  src={profileImageUrl}
                  alt="User avatar"
                  className="h-12 w-12 rounded-xl object-cover sm:h-14 sm:w-14"
                  onError={() => setAvatarError(true)}
                />
              )}
              <div className="flex-1"><h2 className="text-lg font-bold">{user?.name ?? 'User'}</h2><p className="text-xs text-slate-500 mt-0.5">{user?.email ?? '-'}</p></div>
            </div>
          </Card>


          <OAuthConfigManager />

          <Card className="p-4">
            <h2 className="text-[16px] font-bold">Connected Storage Accounts</h2>
            <div className="mt-3.5 grid gap-3">
              {accounts.length === 0 ? <p className="text-xs text-slate-500">No connected storage account yet.</p> : <>
                <label className="grid gap-1.5 text-xs font-semibold text-slate-500">Choose Account<select className="h-10 rounded-xl border border-slate-200 bg-white px-3 text-sm focus:outline-none" value={selectedAccount?.id ?? ''} onChange={(event) => setSelectedAccountId(event.target.value)}>{accounts.map((account) => <option key={account.id} value={account.id}>{providerLabel(account.provider)} - {account.displayName || account.email} ({account.status})</option>)}</select></label>
                {selectedAccount ? <div className="rounded-xl bg-slate-50 p-3 dark:bg-slate-900/60 border border-slate-100 dark:border-slate-800">
                  <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                    <div className="min-w-0"><p className="break-all font-semibold text-sm">{selectedAccount.displayName || selectedAccount.email}</p><p className="text-xs text-slate-500 mt-0.5">{providerLabel(selectedAccount.provider)} · {selectedAccount.status}{selectedAccount.needsReconnect ? <span className="ml-2 inline-flex items-center rounded-full bg-red-100 px-2 py-0.5 text-[11px] font-bold text-red-700">Token expired — klik Connect Drive untuk reconnect</span> : null}</p></div>
                    <div className="grid grid-cols-2 gap-2 sm:flex"><Button className="w-full" size="sm" variant="outline" onClick={() => sync(selectedAccount.id)} disabled={syncingAccountId === selectedAccount.id}><RefreshCw className={syncingAccountId === selectedAccount.id ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />{syncingAccountId === selectedAccount.id ? 'Syncing...' : 'Sync'}</Button><Button className="w-full" size="sm" variant="danger" onClick={() => setAccountToDisconnect(selectedAccount)}><Trash2 className="h-4 w-4" />Disconnect</Button></div>
                  </div>
                  <div className="mt-3 grid grid-cols-3 gap-2 text-center text-xs">
                    <div className="rounded-xl bg-white dark:bg-slate-950 p-2 border border-slate-100 dark:border-slate-800"><p className="font-extrabold text-slate-950">{formatBytes(selectedAccount.storageAccount?.usedBytes)}</p><p className="mt-0.5 text-[10px] text-slate-500">Used</p></div>
                    <div className="rounded-xl bg-white dark:bg-slate-950 p-2 border border-slate-100 dark:border-slate-800"><p className="font-extrabold text-slate-950">{storageLimitLabel(selectedAccount)}</p><p className="mt-0.5 text-[10px] text-slate-500">Total</p></div>
                    <div className="rounded-xl bg-white dark:bg-slate-950 p-2 border border-slate-100 dark:border-slate-800"><p className="font-extrabold text-slate-950">{availableLabel(selectedAccount)}</p><p className="mt-0.5 text-[10px] text-slate-500">Free</p></div>
                  </div>
                </div> : null}
              </>}
            </div>
          </Card>

          <Card className="overflow-hidden p-3.5">
            <div className="flex flex-col gap-3.5 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <div className="flex items-center gap-2.5"><Cloud className="h-5 w-5 text-blue-600" /><h2 className="text-[16px] font-bold">Google Drive</h2></div>
                <p className="mt-1 text-[13px] text-slate-500">Connect one or more Google Drive accounts. PanDrive will route uploads to account with enough space.</p>
              </div>
              <Button className="w-full sm:w-32" size="sm" onClick={connectDrive} disabled={connecting}><Link2 className="h-4 w-4" />{connecting ? 'Opening...' : 'Connect Drive'}</Button>
            </div>
          </Card>

          <OAuthConfigManager />

          <Card className="overflow-hidden p-3.5">
            <div className="flex flex-col gap-3.5 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <div className="flex items-center gap-2.5">
                  <RefreshCw className="h-5 w-5 text-blue-600" />
                  <h2 className="text-[16px] font-bold">System Update</h2>
                </div>
                <p className="mt-1 text-[13px] text-slate-500">
                  Pull the latest code from GitHub. Dev servers will automatically restart.
                </p>
              </div>
              <Button
                className="w-full sm:w-32"
                variant="outline"
                size="sm"
                onClick={runSystemUpdate}
                disabled={updatingSystem}
              >
                <RefreshCw className={updatingSystem ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />
                {updatingSystem ? 'Updating...' : 'Update Code'}
              </Button>
            </div>
          </Card>

          <Card className="overflow-hidden p-3.5">
            <div className="flex flex-col gap-4">
              <div className="flex items-center justify-between border-b border-slate-100 dark:border-slate-800 pb-3">
                <div className="flex items-center gap-2.5">
                  <Database className="h-5 w-5 text-blue-600" />
                  <h2 className="text-[16px] font-bold">Backup & Restore Database</h2>
                </div>
                <span className="text-[11px] text-slate-400 font-semibold uppercase tracking-wider">SQLite Local Database</span>
              </div>

              <div className="grid gap-5 sm:grid-cols-2">
                {/* Download Backup Section (Translucent Green Glass) */}
                <div className="rounded-2xl bg-emerald-500/5 dark:bg-emerald-500/10 border border-emerald-500/20 dark:border-emerald-500/30 hover:border-emerald-500/40 hover:bg-emerald-500/10 transition-all duration-300 p-5 flex flex-col justify-between shadow-sm relative overflow-hidden group">
                  <div className="flex items-start gap-4">
                    <div className="h-10 w-10 shrink-0 rounded-xl bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 flex items-center justify-center">
                      <HardDrive className="h-5 w-5" />
                    </div>
                    <div>
                      <h3 className="text-sm font-bold text-slate-900 dark:text-slate-100">Download Database Backup</h3>
                      <p className="mt-1 text-[12px] text-slate-500 dark:text-slate-400 leading-normal">
                        Save a copy of your active database containing accounts, virtual folders, file metadata, and configurations.
                      </p>
                    </div>
                  </div>
                  <button
                    className="mt-5 w-full h-11 rounded-xl bg-gradient-to-r from-emerald-500 to-teal-600 hover:from-emerald-400 hover:to-teal-500 text-white font-bold shadow-md shadow-emerald-500/10 hover:shadow-emerald-500/20 border-0 transition-all duration-300 transform active:scale-[0.98] disabled:opacity-50 disabled:pointer-events-none flex items-center justify-center gap-2 cursor-pointer"
                    onClick={downloadBackup}
                    disabled={downloadingBackup}
                    style={{ color: '#ffffff' }}
                  >
                    <HardDrive className="h-4 w-4" style={{ color: '#ffffff' }} />
                    <span style={{ color: '#ffffff' }}>{downloadingBackup ? 'Downloading...' : 'Download Backup'}</span>
                  </button>
                </div>

                {/* Restore Backup Section (Translucent Orange Glass) */}
                <div className="rounded-2xl bg-amber-500/5 dark:bg-amber-500/10 border border-amber-500/20 dark:border-amber-500/30 hover:border-amber-500/40 hover:bg-amber-500/10 transition-all duration-300 p-5 flex flex-col justify-between shadow-sm relative overflow-hidden group">
                  <div className="flex items-start gap-4">
                    <div className="h-10 w-10 shrink-0 rounded-xl bg-amber-500/15 text-amber-600 dark:text-amber-400 flex items-center justify-center">
                      <RefreshCw className="h-5 w-5" />
                    </div>
                    <div>
                      <h3 className="text-sm font-bold text-slate-900 dark:text-slate-100">Restore Database Backup</h3>
                      <p className="mt-1 text-[12px] text-slate-500 dark:text-slate-400 leading-normal">
                        Upload a previously downloaded PanDrive backup file to replace the active database.
                      </p>
                    </div>
                  </div>

                  <div className="mt-5 grid gap-3">
                    <input
                      type="file"
                      accept=".db"
                      onChange={handleRestoreFileChange}
                      className="block w-full text-xs text-slate-500 file:mr-3 file:py-1.5 file:px-3 file:rounded-xl file:border-0 file:text-[11px] file:font-extrabold file:bg-amber-500/15 file:text-amber-700 dark:file:text-amber-300 hover:file:bg-amber-500/20 cursor-pointer border border-amber-500/20 dark:border-amber-500/30 rounded-xl p-1 bg-amber-500/5"
                    />
                    {restoreFile ? (
                      <button
                        className="w-full h-11 rounded-xl bg-gradient-to-r from-amber-500 to-orange-600 hover:from-amber-400 hover:to-orange-500 text-white font-bold shadow-md shadow-amber-500/10 hover:shadow-amber-500/20 border-0 transition-all duration-300 transform active:scale-[0.98] disabled:opacity-50 disabled:pointer-events-none flex items-center justify-center gap-2 cursor-pointer"
                        onClick={restoreBackup}
                        disabled={restoringBackup}
                        style={{ color: '#ffffff' }}
                      >
                        <RefreshCw className={restoringBackup ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} style={{ color: '#ffffff' }} />
                        <span style={{ color: '#ffffff' }}>{restoringBackup ? 'Restoring & Restarting...' : 'Restore Backup'}</span>
                      </button>
                    ) : (
                      <button
                        className="w-full h-11 rounded-xl bg-slate-100 dark:bg-slate-800/50 text-slate-400 dark:text-slate-600 border border-slate-200/50 dark:border-slate-800/50 cursor-not-allowed flex items-center justify-center gap-2"
                        disabled
                      >
                        <RefreshCw className="h-4 w-4 text-slate-400 dark:text-slate-600" />
                        <span>Restore Backup</span>
                      </button>
                    )}
                  </div>
                </div>
              </div>

              {restoreMessage && (
                <p className={restoreSuccess ? "rounded-xl bg-emerald-50 p-3 text-xs font-semibold mt-1 text-emerald-700" : "rounded-xl bg-red-50 p-3 text-xs font-semibold mt-1 text-red-700"}>
                  {restoreMessage}
                </p>
              )}
            </div>
          </Card>
        </div>
        <div className="grid gap-3 sm:grid-cols-3 lg:grid-cols-1 lg:gap-3">
          <Card className="p-4"><HardDrive className="h-5 w-5 text-blue-600" /><h2 className="mt-2 text-[14px] font-bold">Storage</h2><p className="mt-1 text-[12px] text-slate-500">Connected accounts: {accounts.length}</p></Card>
          <Card className="p-4"><Bell className="h-5 w-5 text-blue-600" /><h2 className="mt-2 text-[14px] font-bold">Notifications</h2><p className="mt-1 text-[12px] text-slate-500">Email and app alerts are active.</p></Card>
          <Card className="p-4"><Globe className="h-5 w-5 text-blue-600" /><h2 className="mt-2 text-[14px] font-bold">Region</h2><p className="mt-1 text-[12px] text-slate-500">Workspace region: local gateway.</p></Card>
        </div>
      </div>
      <DummyModal open={Boolean(accountToDisconnect)} title="Disconnect storage?" description="This will remove this storage account from PanDrive. Existing file records for this account may no longer be usable." onClose={() => setAccountToDisconnect(null)}>
        <div className="grid gap-4">
          <div className="rounded-xl bg-slate-50 p-4 text-sm text-slate-600">
            <p className="font-semibold text-slate-950">{accountToDisconnect?.email}</p>
            <p className="mt-1">Used storage: {formatBytes(accountToDisconnect?.storageAccount?.usedBytes)}</p>
          </div>
          <div className="grid gap-3 sm:flex sm:justify-end">
            <Button variant="outline" onClick={() => setAccountToDisconnect(null)} disabled={Boolean(disconnectingAccountId)}>Cancel</Button>
            <Button variant="danger" onClick={disconnect} disabled={Boolean(disconnectingAccountId)}><Trash2 className="h-4 w-4" />{disconnectingAccountId ? 'Disconnecting...' : 'Disconnect'}</Button>
          </div>
        </div>
      </DummyModal>

      <DummyModal
        open={updateModalOpen}
        title={updateModalTitle}
        description={
          updateFinished
            ? (updateSuccess ? 'System updated successfully' : 'Update failed')
            : 'Live installation logs'
        }
        className="max-w-2xl"
        onClose={() => {
          if (!updateFinished) {
            if (!confirm('The update is still running in the background. Close log viewer?')) {
              return
            }
          }
          setUpdateModalOpen(false)
          setIsPollingLog(false)
          if (updateFinished && updateSuccess) {
            window.location.reload()
          }
        }}
      >
        <div className="grid gap-4">
          <div
            ref={logContainerRef}
            className="relative rounded-xl bg-slate-950 p-4 font-mono text-xs text-slate-300 leading-relaxed border border-slate-800 h-80 overflow-y-auto select-text"
          >
            <pre className="whitespace-pre-wrap">{updateLog}</pre>
            {!updateFinished && (
              <div className="mt-3 flex items-center gap-2 text-blue-400">
                <RefreshCw className="h-3.5 w-3.5 animate-spin" />
                <span>
                  {reconnectCount > 0
                    ? `Rebooting server and reconnecting... (attempt ${reconnectCount})`
                    : 'Installing updates...'}
                </span>
              </div>
            )}
          </div>
          <div className="flex justify-end gap-2">
            <Button
              variant="outline"
              onClick={() => {
                if (!updateFinished) {
                  if (!confirm('The update is still running. Close log viewer?')) return
                }
                setUpdateModalOpen(false)
                setIsPollingLog(false)
                if (updateFinished && updateSuccess) {
                  window.location.reload()
                }
              }}
            >
              Close
            </Button>
            {updateFinished && updateSuccess && (
              <Button onClick={() => window.location.reload()}>
                Reload Page
              </Button>
            )}
          </div>
        </div>
      </DummyModal>
    </>
  )
}
