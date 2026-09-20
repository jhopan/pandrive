import { lazy, Suspense } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { ProtectedRoute } from '@/components/auth/ProtectedRoute'
import { DriveLayout } from '@/layouts/DriveLayout'
import { LoginPage } from '@/pages/LoginPage'
import { GoogleAuthPage } from '@/pages/GoogleAuthPage'
import GoogleConnectedPage from '@/pages/GoogleConnectedPage'
import { RegisterPage } from '@/pages/RegisterPage'
import { UploadProvider } from '@/context/UploadContext'

const AllFilesPage = lazy(() => import('@/pages/AllFilesPage').then(({ AllFilesPage }) => ({ default: AllFilesPage })))
const QuotaTrackerPage = lazy(() => import('@/pages/QuotaTrackerPage').then(({ QuotaTrackerPage }) => ({ default: QuotaTrackerPage })))
const SettingsPage = lazy(() => import('@/pages/SettingsPage').then(({ SettingsPage }) => ({ default: SettingsPage })))
const RecentPage = lazy(() => import('@/pages/RecentPage').then(({ RecentPage }) => ({ default: RecentPage })))
const StarredPage = lazy(() => import('@/pages/StarredPage').then(({ StarredPage }) => ({ default: StarredPage })))
const HealthPage = lazy(() => import('@/pages/HealthPage').then(({ HealthPage }) => ({ default: HealthPage })))
const SharedPage = lazy(() => import('@/pages/SharedPage').then(({ SharedPage }) => ({ default: SharedPage })))
const UploadsPage = lazy(() => import('@/pages/UploadsPage').then(({ UploadsPage }) => ({ default: UploadsPage })))
const ActivityPage = lazy(() => import('@/pages/ActivityPage').then(({ ActivityPage }) => ({ default: ActivityPage })))
const DuplicatesPage = lazy(() => import('@/pages/DuplicatesPage').then(({ DuplicatesPage }) => ({ default: DuplicatesPage })))
const StoragePage = lazy(() => import('@/pages/StoragePage').then(({ StoragePage }) => ({ default: StoragePage })))
const TrashPage = lazy(() => import('@/pages/TrashPage').then(({ TrashPage }) => ({ default: TrashPage })))

function PageLoader() {
  return <main className="grid min-h-screen place-items-center text-sm font-semibold text-slate-500">Loading PanDrive…</main>
}

function App() {
  return (
    <UploadProvider>
      <Suspense fallback={<PageLoader />}>
        <Routes>
          <Route path="login" element={<LoginPage />} />
          <Route path="register" element={<RegisterPage />} />
          <Route path="google-auth" element={<GoogleAuthPage />} />
          <Route path="google-connected" element={<GoogleConnectedPage />} />
          <Route element={<ProtectedRoute />}>
            <Route element={<DriveLayout />}>
              <Route index element={<Navigate to="/all-files" replace />} />
              <Route path="all-files" element={<AllFilesPage />} />
              <Route path="quota" element={<QuotaTrackerPage />} />
              <Route path="trash" element={<TrashPage />} />
              <Route path="duplicates" element={<DuplicatesPage />} />
              <Route path="storage" element={<StoragePage />} />
              <Route path="activity" element={<ActivityPage />} />
              <Route path="shared" element={<SharedPage />} />
              <Route path="starred" element={<StarredPage />} />
              <Route path="recent" element={<RecentPage />} />
              <Route path="uploads" element={<UploadsPage />} />
              <Route path="health" element={<HealthPage />} />
              <Route path="settings" element={<SettingsPage />} />
            </Route>
          </Route>
          <Route path="*" element={<Navigate to="/all-files" replace />} />
        </Routes>
      </Suspense>
    </UploadProvider>
  )
}

export default App
