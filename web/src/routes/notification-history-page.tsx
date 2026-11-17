import { useState } from "react";
import { useTranslation } from "react-i18next";
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, CircleDashed, History, Loader2, RotateCcw, Skull, Timer } from "lucide-react";
import {
  listNotifications,
  retryNotification,
  type NotificationItem,
  type NotificationStatus,
} from "@/lib/api/notifications";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
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
const fmt = (iso: string) => formatDateTime(dayjs(iso).valueOf());

const STATUS_ICONS: Record<NotificationStatus, React.ReactNode> = {
  pending: <Timer className="size-4 text-muted-foreground" />,
  processing: <CircleDashed className="size-4 text-muted-foreground" />,
  delivered: <CheckCircle2 className="size-4 text-primary" />,
  dead: <Skull className="size-4 text-destructive" />,
};

export function NotificationHistoryPage() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [status, setStatus] = useState<NotificationStatus | "">("");
  const [before, setBefore] = useState<number | undefined>(undefined);

  const items = useQuery({
    queryKey: ["notifications", { status: status || undefined, before }],
    queryFn: ({ signal }) =>
      listNotifications({ status: status || undefined, before_id: before, limit: PAGE_SIZE }, signal),
    placeholderData: keepPreviousData,
  });

  const retry = useMutation({
    mutationFn: (id: number) => retryNotification(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["notifications"] }),
  });

  const rows: NotificationItem[] = items.data ?? [];
  const hasMore = rows.length === PAGE_SIZE;

  return (
    <PageContainer>
      <div className="mb-4 flex items-center gap-2">
        <Select
          value={status}
          onValueChange={(value) => {
            setStatus(value as NotificationStatus | "");
            setBefore(undefined);
          }}
        >
          <SelectTrigger className="w-44" aria-label={t("notifications.filterStatus")}>
            <SelectValue placeholder={t("notifications.filterStatus")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="">{t("notifications.statusAll")}</SelectItem>
            <SelectItem value="pending">{t("notifications.statusPending")}</SelectItem>
            <SelectItem value="processing">{t("notifications.statusProcessing")}</SelectItem>
            <SelectItem value="delivered">{t("notifications.statusDelivered")}</SelectItem>
            <SelectItem value="dead">{t("notifications.statusDead")}</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <Card>
        <CardContent className="p-0">
          {items.isLoading ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
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
                            {retry.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCcw className="size-3.5" />}
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
      {hasMore && (
        <div className="mt-4 flex justify-end">
          <Button
            variant="outline"
            size="sm"
            disabled={items.isLoading}
            onClick={() => {
              const oldest = rows[rows.length - 1];
              if (!oldest) return;
              setBefore(oldest.id);
            }}
          >
            {t("notifications.older")}
          </Button>
        </div>
      )}
    </PageContainer>
  );
}
