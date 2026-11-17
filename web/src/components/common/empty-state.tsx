import type { ReactNode } from "react";

/** Centered icon + title + description, optional action. Shared across pages. */
export function EmptyState({
  icon,
  title,
  desc,
  action,
  className = "py-16",
}: {
  icon: ReactNode;
  title: string;
  desc?: string;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div className={`flex flex-col items-center justify-center gap-3 text-center ${className}`}>
      <div className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground">
        {icon}
      </div>
      <div className="text-base font-medium">{title}</div>
      {desc && <p className="max-w-md break-words text-sm text-muted-foreground">{desc}</p>}
      {action}
    </div>
  );
}
