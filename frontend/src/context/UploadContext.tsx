import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { API_URL, apiFetch } from '@/lib/api'
import { getAccessToken } from '@/lib/auth'

export type UploadProgressStatus = 'uploading' | 'done' | 'error' | 'partial' | 'paused'
export type UploadProgressFile = { name: string; size: number; percent: number; status: UploadProgressStatus }
export type UploadProgressState = { open: boolean; fileName: string; percent: number; status: UploadProgressStatus; files: UploadProgressFile[] }

type ResumableSession = { sessionId: string; file: File; folderId?: string | null; targetAccountId?: string | null }

type StoredSession = { sessionId: string; fileName: string; fileSize: number; folderId?: string | null; targetAccountId?: string | null }

const STORAGE_KEY = 'pandrive:upload-sessions'

type UploadContextType = {
  uploadProgress: UploadProgressState
  setUploadProgress: React.Dispatch<React.SetStateAction<UploadProgressState>>
  uploadFiles: (files: File[], folderId: string | null, targetAccountId?: string | null) => Promise<void>
  retryFailedUpload: (fileName: string) => Promise<void>
  pausedFiles: string[]
  pauseFile: (fileName: string) => void
  resumeFile: (fileName: string) => Promise<void>
  resumableSessions: Record<string, ResumableSession>
}

const UploadContext = createContext<UploadContextType | undefined>(undefined)

function loadStoredSessions(): Record<string, StoredSession> {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}')
  } catch {
    return {}
  }
}

function saveStoredSessions(sessions: Record<string, StoredSession>) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(sessions))
}

export function UploadProvider({ children }: { children: ReactNode }) {
  const [uploadProgress, setUploadProgress] = useState<UploadProgressState>({
    open: false,
    fileName: '',
    percent: 0,
    status: 'uploading',
    files: []
  })
  const [resumableSessions, setResumableSessions] = useState<Record<string, ResumableSession>>({})
  const [pausedFiles, setPausedFiles] = useState<string[]>([])
  // name -> AbortController for in-flight chunk fetch
  const abortControllers = new Map<string, AbortController>()

  // Restore stored sessions on mount (files can't persist, but session resumability info does)
  useEffect(() => {
    const stored = loadStoredSessions()
    if (Object.keys(stored).length === 0) return
    setUploadProgress({
      open: true,
      fileName: Object.keys(stored).length === 1 ? Object.keys(stored)[0] : `${Object.keys(stored).length} files`,
      percent: 0,
      status: 'paused',
      files: Object.values(stored).map(s => ({ name: s.fileName, size: s.fileSize, percent: 0, status: 'paused' }))
    })
  }, [])

  function persistSession(fileName: string, session: StoredSession | null) {
    const stored = loadStoredSessions()
    if (session) stored[fileName] = session
    else delete stored[fileName]
    saveStoredSessions(stored)
  }

  // uploadSplitSequential streams a file across multiple accounts when no single
  // account can hold it: backend plans the parts (greedy largest-first), the browser
  // uploads each part with the same resumable chunk endpoint, in order.
  async function uploadSplitSequential(
    file: File,
    folderId: string | null,
    onProgress: (percent: number) => void,
    controller: AbortController
  ) {
    const CHUNK_SIZE = 32 * 1024 * 1024
    const resp = await apiFetch<{ splitId: string; parts: Array<{ index: string; sizeBytes: string; sessionId: string }> }>('/uploads/split-init', {
      method: 'POST',
      body: JSON.stringify({
        fileName: file.name,
        mimeType: file.type || 'application/octet-stream',
        sizeBytes: String(file.size),
        folderId: folderId || undefined
      })
    })
    const parts = resp.parts.map(p => ({ ...p, size: Number(p.sizeBytes) }))
    const totalSize = parts.reduce((sum, p) => sum + p.size, 0)
    let fileOffset = 0
    for (const part of parts) {
      let partOffset = 0
      while (partOffset < part.size) {
        if (controller.signal.aborted) {
          abortControllers.delete(file.name)
          throw Object.assign(new Error('Upload paused'), { name: 'PauseError' })
        }
        const endOffset = Math.min(partOffset + CHUNK_SIZE, part.size)
        const chunk = file.slice(fileOffset, fileOffset + (endOffset - partOffset))
        const response = await fetch(`${API_URL}/uploads/resumable/chunk/${part.sessionId}`, {
          method: 'PUT',
          headers: {
            'Authorization': `Bearer ${getAccessToken()}`,
            'Content-Range': `bytes ${partOffset}-${endOffset - 1}/${part.size}`,
            'Content-Length': String(chunk.size)
          },
          body: chunk,
          signal: controller.signal
        })
        if (!response.ok) throw new Error('Split chunk upload failed')
        const resData = await response.json() as { status: string; offset?: string }
        if (resData.status === 'completed') break
        partOffset = Number(resData.offset)
        // Byte position in the logical file = bytes of earlier parts + this part's offset.
        fileOffset = sumBefore(parts, part.index) + partOffset
        onProgress(Math.min(99, Math.round((fileOffset / totalSize) * 100)))
      }
    }
    onProgress(100)
  }

  function sumBefore(parts: Array<{ index: string; size: number }>, index: string): number {
    let sum = 0
    for (const p of parts) {
      if (p.index === index) break
      sum += p.size
    }
    return sum
  }

  async function uploadSingleFileResumable(
    file: File,
    folderId: string | null,
    onProgress: (percent: number) => void,
    sessionIdToRetry?: string,
    targetAccountId?: string | null
  ) {
    const CHUNK_SIZE = 32 * 1024 * 1024 // 32MB chunks (multiple of 256KB, Google recommends 8MB+)
    let sessionId = sessionIdToRetry || ''
    let startOffset = 0

    const controller = new AbortController()
    abortControllers.set(file.name, controller)

    // Pre-save session parameters so retry/pause-resume works even if init fails
    setResumableSessions(prev => ({
      ...prev,
      [file.name]: { sessionId, file, folderId, targetAccountId }
    }))

    // 1. Initialize or get status
    if (!sessionId) {
      let initData: { sessionId: string; provider: string }
      try {
        initData = await apiFetch<{ sessionId: string; provider: string }>('/uploads/resumable/init', {
          method: 'POST',
          body: JSON.stringify({
            fileName: file.name,
            mimeType: file.type || 'application/octet-stream',
            sizeBytes: String(file.size),
            folderId: folderId || undefined,
            targetAccountId: targetAccountId || undefined
          })
        })
      } catch (initErr) {
        // No single account fits: try a multi-account split (backend decides feasibility).
        const code = (initErr as { code?: string })?.code || ''
        if (code !== 'NO_SPLIT_POSSIBLE' && code !== 'NO_ACCOUNT_WITH_ENOUGH_SPACE') throw initErr
        await uploadSplitSequential(file, folderId, onProgress, controller)
        return
      }
      sessionId = initData.sessionId
      setResumableSessions(prev => ({
        ...prev,
        [file.name]: { sessionId, file, folderId, targetAccountId }
      }))
      persistSession(file.name, {
        sessionId,
        fileName: file.name,
        fileSize: file.size,
        folderId,
        targetAccountId: targetAccountId ?? undefined
      })
    } else {
      const statusData = await apiFetch<{ status: string; offset: string }>(`/uploads/resumable/status/${sessionId}`)
      startOffset = Number(statusData.offset)
      if (statusData.status === 'completed') {
        abortControllers.delete(file.name)
        onProgress(100)
        return
      }
    }

    // 2. Upload chunk by chunk (checks pause flag between chunks)
    while (startOffset < file.size) {
      if (controller.signal.aborted) {
        abortControllers.delete(file.name)
        throw Object.assign(new Error('Upload paused'), { name: 'PauseError' })
      }
      const endOffset = Math.min(startOffset + CHUNK_SIZE, file.size)
      const chunk = file.slice(startOffset, endOffset)

      const response = await fetch(`${API_URL}/uploads/resumable/chunk/${sessionId}`, {
        method: 'PUT',
        headers: {
          'Authorization': `Bearer ${getAccessToken()}`,
          'Content-Range': `bytes ${startOffset}-${endOffset - 1}/${file.size}`,
          'Content-Length': String(chunk.size)
        },
        body: chunk,
        signal: controller.signal
      })

      if (!response.ok) {
        abortControllers.delete(file.name)
        throw new Error('Chunk upload failed')
      }

      const resData = await response.json() as { status: string; offset?: string }
      if (resData.status === 'completed') {
        abortControllers.delete(file.name)
        persistSession(file.name, null) // done -> clear stored session
        onProgress(100)
        break
      }

      startOffset = Number(resData.offset)
      const percent = Math.min(99, Math.round((startOffset / file.size) * 100))
      onProgress(percent)
    }
    abortControllers.delete(file.name)
  }

  async function uploadFiles(filesToUpload: File[], targetFolderId: string | null, targetAccountId?: string | null) {
    if (filesToUpload.length === 0) return

    setUploadProgress({
      open: true,
      fileName: filesToUpload.length === 1 ? filesToUpload[0].name : `${filesToUpload.length} files`,
      percent: 0,
      status: 'uploading',
      files: filesToUpload.map(f => ({ name: f.name, size: f.size, percent: 0, status: 'uploading' }))
    })

    for (let i = 0; i < filesToUpload.length; i++) {
      const file = filesToUpload[i]
      try {
        await uploadSingleFileResumable(file, targetFolderId, (filePercent) => {
          setUploadProgress((current) => {
            const nextFiles = [...current.files]
            if (nextFiles[i]) {
              nextFiles[i] = { ...nextFiles[i], percent: filePercent, status: filePercent >= 100 ? 'done' : 'uploading' }
            }
            const overallPercent = Math.round(nextFiles.reduce((sum, f) => sum + f.percent, 0) / nextFiles.length)
            return { ...current, percent: overallPercent, files: nextFiles }
          })
        }, undefined, targetAccountId)
      } catch (err) {
        const isPause = (err as Error)?.name === 'PauseError'
        console[isPause ? 'info' : 'error'](isPause ? 'Upload paused:' : 'File upload failed:', file.name, err)
        setUploadProgress((current) => {
          const nextFiles = [...current.files]
          if (nextFiles[i]) {
            nextFiles[i] = { ...nextFiles[i], status: isPause ? 'paused' : 'error' }
          }
          const overallPercent = Math.round(nextFiles.reduce((sum, f) => sum + f.percent, 0) / nextFiles.length)
          return {
            ...current,
            status: isPause ? 'paused' : 'partial',
            percent: overallPercent,
            files: nextFiles
          }
        })
        if (isPause) break // stop the sequential queue; resume continues from here
      }
    }

    window.dispatchEvent(new Event('pandrive:storage-changed'))
    window.dispatchEvent(new Event('pandrive:upload-completed'))
  }

  function pauseFile(fileName: string) {
    setPausedFiles(prev => prev.includes(fileName) ? prev : [...prev, fileName])
    const controller = abortControllers.get(fileName)
    if (controller) controller.abort()
    setUploadProgress((current) => ({
      ...current,
      status: 'paused',
      files: current.files.map(f => f.name === fileName ? { ...f, status: 'paused' as const } : f)
    }))
  }

  async function resumeUpload(fileName: string, session: ResumableSession) {
    setPausedFiles(prev => prev.filter(n => n !== fileName))
    setUploadProgress((current) => ({
      ...current,
      status: 'uploading',
      files: current.files.map(f => f.name === fileName ? { ...f, status: 'uploading' as const } : f)
    }))
    try {
      await uploadSingleFileResumable(session.file, session.folderId || null, (filePercent) => {
        setUploadProgress((current) => {
          const nextFiles = [...current.files]
          const idx = nextFiles.findIndex(f => f.name === fileName)
          if (nextFiles[idx]) {
            nextFiles[idx] = { ...nextFiles[idx], percent: filePercent, status: filePercent >= 100 ? 'done' : 'uploading' }
          }
          const overallPercent = Math.round(nextFiles.reduce((sum, f) => sum + f.percent, 0) / nextFiles.length)
          const allDone = nextFiles.every(f => f.status === 'done')
          return { ...current, percent: overallPercent, status: allDone ? 'done' : 'uploading', files: nextFiles }
        })
      }, session.sessionId || undefined, session.targetAccountId)
      window.dispatchEvent(new Event('pandrive:storage-changed'))
      window.dispatchEvent(new Event('pandrive:upload-completed'))
    } catch (err) {
      const isPause = (err as Error)?.name === 'PauseError'
      setUploadProgress((current) => ({
        ...current,
        status: isPause ? 'paused' : 'partial',
        files: current.files.map(f => f.name === fileName
          ? { ...f, status: isPause ? 'paused' as const : 'error' as const }
          : f)
      }))
    }
  }

  // Resume from paused in-memory session (file object still alive)
  async function resumeFile(fileName: string) {
    const session = resumableSessions[fileName]
    if (session) {
      await resumeUpload(fileName, session)
      return
    }
    // Cross-refresh resume: file object lost, ask user to re-pick it
    const stored = loadStoredSessions()[fileName]
    if (stored) {
      const input = document.createElement('input')
      input.type = 'file'
      input.onchange = async () => {
        const picked = input.files?.[0]
        if (!picked || picked.size !== stored.fileSize) {
          alert(`Please select the original file: ${stored.fileName} (${stored.fileSize} bytes)`)
          return
        }
        await resumeUpload(fileName, { sessionId: stored.sessionId, file: picked, folderId: stored.folderId ?? null, targetAccountId: stored.targetAccountId ?? null })
      }
      input.click()
    }
  }

  async function retryFailedUpload(fileName: string) {
    const session = resumableSessions[fileName]
    if (!session) return

    setUploadProgress((current) => {
      const nextFiles = current.files.map(f => f.name === fileName ? { ...f, status: 'uploading' as const } : f)
      return { ...current, status: 'uploading', files: nextFiles }
    })

    try {
      const fileIndex = uploadProgress.files.findIndex(f => f.name === fileName)
      await uploadSingleFileResumable(session.file, session.folderId || null, (filePercent) => {
        setUploadProgress((current) => {
          const nextFiles = [...current.files]
          if (nextFiles[fileIndex]) {
            nextFiles[fileIndex] = { ...nextFiles[fileIndex], percent: filePercent, status: filePercent >= 100 ? 'done' : 'uploading' }
          }
          const overallPercent = Math.round(nextFiles.reduce((sum, f) => sum + f.percent, 0) / nextFiles.length)
          const allDone = nextFiles.every(f => f.status === 'done')
          return { ...current, percent: overallPercent, status: allDone ? 'done' : 'uploading', files: nextFiles }
        })
      }, session.sessionId, session.targetAccountId)

      window.dispatchEvent(new Event('pandrive:storage-changed'))
      window.dispatchEvent(new Event('pandrive:upload-completed'))
    } catch (err) {
      const isPause = (err as Error)?.name === 'PauseError'
      setUploadProgress((current) => {
        const nextFiles = current.files.map(f => f.name === fileName ? { ...f, status: isPause ? 'paused' as const : 'error' as const } : f)
        return { ...current, status: isPause ? 'paused' : 'partial', files: nextFiles }
      })
    }
  }

  return (
    <UploadContext.Provider value={{ uploadProgress, setUploadProgress, uploadFiles, retryFailedUpload, pausedFiles, pauseFile, resumeFile, resumableSessions }}>
      {children}
    </UploadContext.Provider>
  )
}

export function useUpload() {
  const context = useContext(UploadContext)
  if (context === undefined) {
    throw new Error('useUpload must be used within an UploadProvider')
  }
  return context
}
