import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  CircleDashed,
  History,
  Loader2,
  RotateCcw,
  ShieldAlert,
  Skull,
  Timer,
} from "lucide-react";
import {
  listNotifications,
  retryNotification,
  type NotificationItem,
  type NotificationStatus,
} from "@/lib/api/notifications";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageAlerts } from "@/lib/auth/permissions";
import { useProject } from "@/lib/project/project-provider";
import {
  INITIAL_CURSOR_PAGINATION,
  canGoBackCursor,
  popCursorPage,
  pushCursorPage,
  type CursorPaginationState,
} from "@/lib/cursor-pagination";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { formatErrorMessage } from "@/lib/api/errors";
import { QueryErrorState } from "@/components/common/query-error-state";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

const PAGE_SIZE = 100;
const ALL_STATUS = "__all__";
const fmt = (iso: string) => formatDateTime(dayjs(iso).valueOf());

const STATUS_ICONS: Record<NotificationStatus, React.ReactNode> = {
  pending: <Timer className="size-4 text-muted-foreground" />,
  processing: <CircleDashed className="size-4 text-muted-foreground" />,
  delivered: <CheckCircle2 className="size-4 text-primary" />,
  dead: <Skull className="size-4 text-destructive" />,
};

export function NotificationHistoryPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const { currentSlug } = useProject();
  const canManage = canManageAlerts(role);
  const queryClient = useQueryClient();

  const [status, setStatus] = useState<NotificationStatus | typeof ALL_STATUS>(ALL_STATUS);
  const [pagination, setPagination] = useState<CursorPaginationState>(INITIAL_CURSOR_PAGINATION);
  const [retryError, setRetryError] = useState<string | null>(null);
  const prevProjectRef = useRef(currentSlug);

  useEffect(() => {
    if (prevProjectRef.current !== currentSlug) {
      prevProjectRef.current = currentSlug;
      setPagination(INITIAL_CURSOR_PAGINATION);
      setRetryError(null);
    }
  }, [currentSlug]);

  const statusParam = status === ALL_STATUS ? undefined : status;

  const items = useQuery({
    queryKey: ["notifications", currentSlug, { status: statusParam, before: pagination.before }],
    queryFn: ({ signal }) =>
      listNotifications(
        { status: statusParam, before_id: pagination.before, limit: PAGE_SIZE },
        signal,
      ),
    placeholderData: keepPreviousData,
    enabled: canManage,
    retry: false,
  });

  const retry = useMutation({
    mutationFn: (id: number) => retryNotification(id),
    onMutate: () => setRetryError(null),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["notifications"] }),
    onError: (err) => setRetryError(formatErrorMessage(err, t("notifications.retryError"))),
  });

  if (!canManage) {
    return (
      <PageContainer>
        <Card>
          <CardContent>
            <EmptyState
              icon={<ShieldAlert className="size-6" />}
              title={t("common.forbiddenTitle")}
              desc={t("notifications.forbiddenDesc")}
            />
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  const rows: NotificationItem[] = items.data ?? [];
  const hasMore = rows.length === PAGE_SIZE;

  function goBack() {
    setPagination((prev) => popCursorPage(prev));
  }

  function goForward() {
    const oldest = rows[rows.length - 1];
    if (!oldest) return;
    setPagination((prev) => pushCursorPage(prev, oldest.id));
  }

  return (
    <PageContainer>
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <Select
          value={status}
          onValueChange={(value) => {
            setStatus(value as NotificationStatus | typeof ALL_STATUS);
            setPagination(INITIAL_CURSOR_PAGINATION);
          }}
        >
          <SelectTrigger className="w-44" aria-label={t("notifications.filterStatus")}>
            <SelectValue placeholder={t("notifications.filterStatus")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL_STATUS}>{t("notifications.statusAll")}</SelectItem>
            <SelectItem value="pending">{t("notifications.statusPending")}</SelectItem>
            <SelectItem value="processing">{t("notifications.statusProcessing")}</SelectItem>
            <SelectItem value="delivered">{t("notifications.statusDelivered")}</SelectItem>
            <SelectItem value="dead">{t("notifications.statusDead")}</SelectItem>
          </SelectContent>
        </Select>
        {retryError && (
          <p className="text-sm text-destructive" role="alert">
            {retryError}
          </p>
        )}
      </div>
      <Card>
        <CardContent className="p-0">
          {items.isLoading ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : items.isError ? (
            <QueryErrorState
              error={items.error}
              forbiddenDesc={t("notifications.forbiddenDesc")}
              onRetry={() => void items.refetch()}
            />
          ) : rows.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("notifications.colWhen")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("notifications.colChannel")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("notifications.colStatus")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("notifications.colAttempts")}
                    </th>
                    <th className="px-4 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((item) => (
                    <tr key={item.id} className="border-b border-border/60 last:border-0">
                      <td className="whitespace-nowrap px-4 py-2.5 text-muted-foreground">
                        {fmt(item.created_at)}
                      </td>
                      <td className="px-4 py-2.5">
                        <span className="font-medium">{item.channel_name || `#${item.channel_id}`}</span>
                        <span className="ml-1.5 text-xs text-muted-foreground">{item.channel_type}</span>
                      </td>
                      <td className="px-4 py-2.5">
                        <span className="inline-flex items-center gap-1.5">
                          {STATUS_ICONS[item.status]}
                          {t(`notifications.status${item.status[0].toUpperCase()}${item.status.slice(1)}`)}
                        </span>
                        {item.last_error && (
                          <span className="mt-0.5 block truncate text-xs text-destructive" title={item.last_error}>
                            {item.last_error}
                          </span>
                        )}
                      </td>
                      <td className="hidden px-4 py-2.5 text-muted-foreground md:table-cell">
                        {item.attempts}
                      </td>
                      <td className="px-4 py-2.5 text-right">
                        {item.status === "dead" && (
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() => retry.mutate(item.id)}
                            disabled={retry.isPending}
                            className="gap-1.5"
                          >
                            {retry.isPending && retry.variables === item.id ? (
                              <Loader2 className="size-3.5 animate-spin" />
                            ) : (
                              <RotateCcw className="size-3.5" />
                            )}
                            {t("notifications.retry")}
                          </Button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <EmptyState icon={<History className="size-6" />} title={t("notifications.empty")} />
          )}
        </CardContent>
      </Card>
      <div className="mt-4 flex items-center justify-end gap-2">
        <Button
          variant="outline"
          size="sm"
          onClick={goBack}
          disabled={!canGoBackCursor(pagination) || items.isLoading}
        >
          <ChevronLeft className="size-4" />
          {t("notifications.newer")}
        </Button>
        <Button
          variant="outline"
          size="sm"
          onClick={goForward}
          disabled={!hasMore || items.isLoading || items.isError}
        >
          {t("notifications.older")}
          <ChevronRight className="size-4" />
        </Button>
      </div>
    </PageContainer>
  );
}
