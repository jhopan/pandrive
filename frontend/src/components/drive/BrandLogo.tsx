import { cn } from '@/lib/utils'

// Single source of truth for the app mark: the PNG shipped in public/ (also the favicon and PWA icon).
export function BrandLogo({ className }: { className?: string }) {
  return (
    <img
      src="/logo.png"
      alt="PanDrive"
      width={40}
      height={40}
      className={cn('h-10 w-10 rounded-xl object-cover shadow-lg shadow-blue-200', className)}
    />
  )
}
