import { avatarColor, avatarInitial } from '@/lib/avatar'
import { cn } from '@/lib/utils'

// Local initials instead of a third-party avatar CDN: nothing to block, nothing to rate-limit.
export function AvatarStack({ count, seed = 'member' }: { count: number; seed?: string }) {
  return (
    <div className="flex -space-x-2">
      {Array.from({ length: count }).map((_, index) => {
        const label = `${seed}-${index}`
        return (
          <span
            key={index}
            title="Member"
            className={cn('flex h-5 w-5 items-center justify-center rounded-full border-2 border-white text-[9px] font-bold text-white')}
            style={{ backgroundColor: avatarColor(label) }}
          >
            {avatarInitial(`${index + 1}`)}
          </span>
        )
      })}
    </div>
  )
}
