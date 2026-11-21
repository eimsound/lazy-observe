import {
  AlertCircle,
  ChevronLeft,
  ChevronRight,
} from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";

const PAGE_SIZE_OPTIONS = [10, 25, 50, 100];

export function IconAction({
  label,
  children,
  disabled,
  onClick,
  className,
}: {
  label: string;
  children: ReactNode;
  disabled?: boolean;
  onClick: () => void;
  className?: string;
}) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      className={cn("size-7 shrink-0", className)}
      title={label}
      aria-label={label}
      disabled={disabled}
      onClick={onClick}
    >
      {children}
    </Button>
  );
}

export function PaginationControls({
  total,
  page,
  pageSize,
  pageSizeLabel,
  onPageChange,
  onPageSizeChange,
}: {
  total: number;
  page: number;
  pageSize: number;
  pageSizeLabel?: string;
  onPageChange: (page: number) => void;
  onPageSizeChange: (pageSize: number) => void;
}) {
  const { t } = useTranslation();
  const safePageSize = Math.max(1, pageSize);
  const pageCount = Math.max(1, Math.ceil(total / safePageSize));
  const effectivePage = Math.min(Math.max(1, page), pageCount);
  const from = total === 0 ? 0 : (effectivePage - 1) * safePageSize + 1;
  const to = Math.min(effectivePage * safePageSize, total);
  const pageSizes = PAGE_SIZE_OPTIONS.includes(safePageSize)
    ? PAGE_SIZE_OPTIONS
    : [...PAGE_SIZE_OPTIONS, safePageSize].sort((left, right) => left - right);

  return (
    <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border/60 px-2 py-2 text-xs text-muted-foreground">
      <span aria-live="polite">
        {t("logs.paginationStatus", { from, to, total })}
      </span>
      <div className="flex items-center gap-2">
        <span className="hidden sm:inline">
          {pageSizeLabel ?? t("logs.rowsPerPage")}
        </span>
        <Select
          value={String(safePageSize)}
          onValueChange={(value) => onPageSizeChange(Number(value))}
        >
          <SelectTrigger
            className="h-8 w-[4.75rem]"
            aria-label={pageSizeLabel ?? t("logs.rowsPerPage")}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {pageSizes.map((option) => (
              <SelectItem key={option} value={String(option)}>
                {option}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-8"
          aria-label={t("logs.previousPage")}
          title={t("logs.previousPage")}
          disabled={effectivePage <= 1}
          onClick={() => onPageChange(effectivePage - 1)}
        >
          <ChevronLeft />
        </Button>
        <span className="min-w-14 text-center tabular-nums text-foreground">
          {effectivePage} / {pageCount}
        </span>
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-8"
          aria-label={t("logs.nextPage")}
          title={t("logs.nextPage")}
          disabled={effectivePage >= pageCount}
          onClick={() => onPageChange(effectivePage + 1)}
        >
          <ChevronRight />
        </Button>
      </div>
    </div>
  );
}

export function EmptyLogs() {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-40 items-center justify-center px-4 text-center text-sm text-muted-foreground">
      {t("logs.emptyTitle")}
    </div>
  );
}

export function ExplorerMessage({
  icon,
  title,
  description,
}: {
  icon: ReactNode;
  title: string;
  description: string;
}) {
  return (
    <div className="flex min-h-40 flex-col items-center justify-center gap-2 px-4 text-center">
      <div className="flex size-10 items-center justify-center rounded-full bg-muted text-muted-foreground [&>svg]:size-5">
        {icon}
      </div>
      <div className="text-sm font-medium">{title}</div>
      <p className="max-w-lg text-xs text-muted-foreground">{description}</p>
    </div>
  );
}

export function ExplorerError({
  title,
  onRetry,
}: {
  title: string;
  onRetry: () => void;
}) {
  const { t } = useTranslation();
  return (
    <div
      className={cn(
        "flex min-h-40 flex-col items-center justify-center gap-2 rounded-md",
        "border border-destructive/30 bg-destructive/5 px-4 text-center",
      )}
    >
      <AlertCircle className="size-5 text-destructive" />
      <div className="text-sm font-medium">{title}</div>
      <p className="max-w-2xl break-words text-xs text-muted-foreground">
        {t("common.queryErrorDesc")}
      </p>
      <Button variant="outline" size="sm" onClick={onRetry}>
        {t("common.retry")}
      </Button>
    </div>
  );
}
