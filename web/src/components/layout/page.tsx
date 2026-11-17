import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/** Shared content wrapper for authenticated pages. */
export function PageContainer({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return <div className={cn("w-full p-6", className)}>{children}</div>;
}

/** Page-level action row; the page title and description are intentionally absent. */
export function PageActions({
  children,
}: {
  children: ReactNode;
}) {
  return (
    <div className="mb-6 flex flex-wrap items-center justify-end gap-2">
      {children}
    </div>
  );
}

