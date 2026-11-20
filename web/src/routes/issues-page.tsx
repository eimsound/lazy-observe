import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import {
  AlertCircle,
  Bug,
  CheckCircle2,
  Database,
  Loader2,
  Search,
  VolumeX,
  RotateCcw,
} from "lucide-react";
import {
  listIssues,
  updateIssueStatus,
  batchUpdateIssueStatus,
  ISSUE_LEVELS,
  type IssueLevel,
  type IssueListResult,
  type IssueSort,
  type IssueStatus,
  type IssueSummary,
} from "@/lib/api/issues";
import { IS_MOCK } from "@/lib/settings";
import { ApiError } from "@/lib/api/client";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageIssues } from "@/lib/auth/permissions";
import { useProject } from "@/lib/project/project-provider";
import { formatErrorMessage } from "@/lib/api/errors";
import { formatRelativeTime } from "@/lib/format";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Sparkline } from "@/components/common/sparkline";
import { IssueLevelBadge } from "@/components/issues/issue-level-badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { queryViewState } from "./query-view-state";

const ALL_LEVELS = "__all__";
const STATUS_TABS: IssueStatus[] = ["open", "resolved", "ignored"];
const STATUS_TAB_KEY: Record<IssueStatus, string> = {
  open: "issues.tabOpen",
  resolved: "issues.tabResolved",
  ignored: "issues.tabIgnored",
};

export function IssuesPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const { currentSlug } = useProject();
  const canManage = canManageIssues(role);
  const queryClient = useQueryClient();

  const [status, setStatus] = useState<IssueStatus>("open");
  const [level, setLevel] = useState<string>(ALL_LEVELS);
  const [qInput, setQInput] = useState("");
  const [q, setQ] = useState("");
  const [sort, setSort] = useState<IssueSort>("last_seen");
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [actionError, setActionError] = useState<string | null>(null);
  const prevProjectRef = useRef(currentSlug);

  useEffect(() => {
    if (prevProjectRef.current !== currentSlug) {
      prevProjectRef.current = currentSlug;
      setPage(1);
      setSelected(new Set());
      setActionError(null);
    }
  }, [currentSlug]);

  const levelParam = level === ALL_LEVELS ? "" : (level as IssueLevel);
  const queryKey = ["issues", currentSlug, status, levelParam, q, sort, page] as const;

  const query = useQuery<IssueListResult, ApiError>({
    queryKey,
    queryFn: ({ signal }) =>
      listIssues({ status, level: levelParam, q, sort, page }, signal),
    retry: false,
    placeholderData: (prev) => prev,
  });

  const mutation = useMutation({
    mutationFn: ({ id, next }: { id: string; next: IssueStatus }) =>
      updateIssueStatus(id, next),
    onMutate: ({ id }) => {
      setActionError(null);
      const previous = queryClient.getQueryData<IssueListResult>(queryKey);
      queryClient.setQueryData<IssueListResult>(queryKey, (old) =>
        old ? { ...old, issues: old.issues.filter((i) => i.id !== id) } : old,
      );
      return { previous };
    },
    onError: (err, _vars, context) => {
      if (context?.previous) {
        queryClient.setQueryData(queryKey, context.previous);
      }
      setActionError(formatErrorMessage(err, t("issues.statusUpdateError")));
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });

  const batchMutation = useMutation({
    mutationFn: ({ ids, next }: { ids: string[]; next: IssueStatus }) =>
      batchUpdateIssueStatus(ids, next),
    onMutate: ({ ids }) => {
      setActionError(null);
      const previous = queryClient.getQueryData<IssueListResult>(queryKey);
      queryClient.setQueryData<IssueListResult>(queryKey, (old) =>
        old ? { ...old, issues: old.issues.filter((i) => !ids.includes(i.id)) } : old,
      );
      return { previous };
    },
    onError: (err, _vars, context) => {
      if (context?.previous) {
        queryClient.setQueryData(queryKey, context.previous);
      }
      setActionError(formatErrorMessage(err, t("issues.statusUpdateError")));
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });

  const issues = query.data?.issues ?? [];

  function resetSelection() {
    setSelected(new Set());
  }

  function changeStatus(status: IssueStatus) {
    setStatus(status);
    setPage(1);
    setActionError(null);
    resetSelection();
  }

  function applyStatus(ids: string[], next: IssueStatus) {
    if (!canManage) return;
    resetSelection();
    if (ids.length === 1) {
      mutation.mutate({ id: ids[0], next });
      return;
    }
    batchMutation.mutate({ ids, next });
  }

  function toggleSelect(id: string) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function toggleSelectAll() {
    setSelected((prev) =>
      prev.size === issues.length ? new Set() : new Set(issues.map((i) => i.id)),
    );
  }

  function runSearch() {
    setQ(qInput.trim());
    setPage(1);
    resetSelection();
  }

  const selectedIds = useMemo(() => [...selected], [selected]);

  return (
    <PageContainer>
      {IS_MOCK && (
        <PageActions>
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("issues.mockBadge")}
          </Badge>
        </PageActions>
      )}

      {/* Status tabs */}
      <div className="mb-4 inline-flex rounded-lg border border-border p-0.5">
        {STATUS_TABS.map((s) => (
          <button
            key={s}
            type="button"
            onClick={() => changeStatus(s)}
            className={cn(
              "rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
              status === s
                ? "bg-primary text-primary-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {t(STATUS_TAB_KEY[s])}
          </button>
        ))}
      </div>

      {/* Filters */}
      <Card className="mb-4">
        <CardContent className="flex flex-col gap-3 p-3 sm:flex-row sm:items-center">
          <div className="relative flex-1">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={qInput}
              onChange={(e) => setQInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") runSearch();
              }}
              placeholder={t("issues.searchPlaceholder")}
              className="pl-8"
            />
          </div>
          <Select
            value={level}
            onValueChange={(v) => {
              setLevel(v);
              setPage(1);
              resetSelection();
            }}
          >
            <SelectTrigger className="w-full sm:w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL_LEVELS}>{t("issues.allLevels")}</SelectItem>
              {ISSUE_LEVELS.map((l) => (
                <SelectItem key={l} value={l}>
                  {t(`issues.level${l[0].toUpperCase()}${l.slice(1)}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select
            value={sort}
            onValueChange={(v) => {
              setSort(v as IssueSort);
              setPage(1);
            }}
          >
            <SelectTrigger className="w-full sm:w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="last_seen">{t("issues.sortLastSeen")}</SelectItem>
              <SelectItem value="count">{t("issues.sortCount")}</SelectItem>
            </SelectContent>
          </Select>
        </CardContent>
      </Card>

      {actionError && (
        <div
          className="mb-3 flex items-center gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
          role="alert"
        >
          <AlertCircle className="size-4 shrink-0" />
          <span>{actionError}</span>
        </div>
      )}

      {/* Bulk action bar */}
      {canManage && selectedIds.length > 0 && (
        <div className="mb-3 flex flex-wrap items-center gap-2 rounded-md border border-border bg-muted/40 px-3 py-2 text-sm">
          <span className="font-medium">
            {t("issues.selectedCount", { count: selectedIds.length })}
          </span>
          <div className="ml-auto flex gap-2">
            {status !== "resolved" && (
              <Button size="sm" variant="outline" onClick={() => applyStatus(selectedIds, "resolved")}>
                <CheckCircle2 className="size-3.5" />
                {t("issues.resolve")}
              </Button>
            )}
            {status !== "ignored" && (
              <Button size="sm" variant="outline" onClick={() => applyStatus(selectedIds, "ignored")}>
                <VolumeX className="size-3.5" />
                {t("issues.ignore")}
              </Button>
            )}
            {status !== "open" && (
              <Button size="sm" variant="outline" onClick={() => applyStatus(selectedIds, "open")}>
                <RotateCcw className="size-3.5" />
                {t("issues.reopen")}
              </Button>
            )}
          </div>
        </div>
      )}

      <IssueListBody
        query={query}
        issues={issues}
        status={status}
        canManage={canManage}
        allSelected={issues.length > 0 && selected.size === issues.length}
        selected={selected}
        onToggleSelect={toggleSelect}
        onToggleSelectAll={toggleSelectAll}
        onApplyStatus={(id, next) => applyStatus([id], next)}
        onRetry={() => query.refetch()}
      />

      {/* Pagination */}
      {query.data && query.data.total > query.data.page_size && (
        <div className="mt-4 flex items-center justify-between text-sm">
          <span className="text-muted-foreground">
            {t("issues.pageInfo", { page: query.data.page, total: query.data.total })}
          </span>
          <div className="flex gap-2">
            <Button
              size="sm"
              variant="outline"
              disabled={page <= 1}
              onClick={() => {
                setPage((p) => Math.max(1, p - 1));
                resetSelection();
              }}
            >
              {t("issues.pagePrev")}
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={page * query.data.page_size >= query.data.total}
              onClick={() => {
                setPage((p) => p + 1);
                resetSelection();
              }}
            >
              {t("issues.pageNext")}
            </Button>
          </div>
        </div>
      )}
    </PageContainer>
  );
}

function IssueListBody({
  query,
  issues,
  status,
  canManage,
  allSelected,
  selected,
  onToggleSelect,
  onToggleSelectAll,
  onApplyStatus,
  onRetry,
}: {
  query: ReturnType<typeof useQuery<IssueListResult, ApiError>>;
  issues: IssueSummary[];
  status: IssueStatus;
  canManage: boolean;
  allSelected: boolean;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  onToggleSelectAll: () => void;
  onApplyStatus: (id: string, next: IssueStatus) => void;
  onRetry: () => void;
}) {
  const { t } = useTranslation();
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: issues.length === 0,
  });

  if (viewState === "pending") {
    return (
      <Card>
        <CardContent className="space-y-2 p-4">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            {t("issues.loading")}
          </div>
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-14 w-full" />
          ))}
        </CardContent>
      </Card>
    );
  }

  if (viewState === "error") {
    return (
      <Card>
        <CardContent className="py-16">
          <EmptyState
            icon={<AlertCircle className="size-6 text-destructive" />}
            title={t("issues.errorTitle")}
            desc={t("common.queryErrorDesc")}
            action={
              <Button variant="outline" size="sm" onClick={onRetry}>
                {t("common.retry")}
              </Button>
            }
          />
        </CardContent>
      </Card>
    );
  }

  if (viewState === "empty") {
    return (
      <Card>
        <CardContent className="py-16">
          <EmptyState
            icon={<Bug className="size-6" />}
            title={t("issues.emptyTitle")}
            desc={t("issues.emptyDesc")}
          />
        </CardContent>
      </Card>
    );
  }

  return (
    <div>
      {viewState === "stale" && (
        <QueryStaleNotice isRetrying={query.isFetching} onRetry={onRetry} />
      )}
      <Card>
        <CardContent className="p-0">
        <div className="flex items-center gap-3 border-b border-border px-3 py-2 text-xs font-medium text-muted-foreground">
          {canManage && (
            <input
              type="checkbox"
              className="size-4 accent-primary"
              checked={allSelected}
              onChange={onToggleSelectAll}
              aria-label={t("common.all")}
            />
          )}
          <span className="flex-1">{t("issues.colIssue")}</span>
        </div>
        <ul>
          {issues.map((issue) => (
            <IssueRow
              key={issue.id}
              issue={issue}
              status={status}
              canManage={canManage}
              checked={selected.has(issue.id)}
              onToggleSelect={() => onToggleSelect(issue.id)}
              onApplyStatus={(next) => onApplyStatus(issue.id, next)}
            />
          ))}
        </ul>
        </CardContent>
      </Card>
    </div>
  );
}

function IssueRow({
  issue,
  status,
  canManage,
  checked,
  onToggleSelect,
  onApplyStatus,
}: {
  issue: IssueSummary;
  status: IssueStatus;
  canManage: boolean;
  checked: boolean;
  onToggleSelect: () => void;
  onApplyStatus: (next: IssueStatus) => void;
}) {
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();

  return (
    <li className="flex items-center gap-3 border-b border-border/60 px-3 py-2.5 last:border-b-0 hover:bg-muted/30">
      {canManage && (
        <input
          type="checkbox"
          className="size-4 accent-primary"
          checked={checked}
          onChange={onToggleSelect}
          aria-label={issue.title}
        />
      )}
      <button
        type="button"
        onClick={() => navigate({ to: "/issues/$issueId", params: { issueId: issue.id } })}
        className="min-w-0 flex-1 text-left"
      >
        <div className="flex items-center gap-2">
          <IssueLevelBadge level={issue.level} />
          <span className="truncate text-sm font-medium hover:underline">{issue.title}</span>
        </div>
        <div className="mt-0.5 flex items-center gap-3 text-xs text-muted-foreground">
          <span>{t("issues.firstSeen", { time: formatRelativeTime(issue.first_seen, i18n.language) })}</span>
          <span>{t("issues.lastSeen", { time: formatRelativeTime(issue.last_seen, i18n.language) })}</span>
        </div>
      </button>

      <div className="hidden text-muted-foreground sm:block">
        <Sparkline data={issue.trend} />
      </div>

      <div className="hidden w-24 shrink-0 text-right sm:block">
        <div className="text-sm font-semibold tabular-nums">{issue.count.toLocaleString()}</div>
        <div className="text-xs text-muted-foreground">{t("issues.users", { count: issue.user_count })}</div>
      </div>

      {canManage && (
        <div className="flex shrink-0 gap-1">
          {status === "open" ? (
            <>
              <RowAction
                title={t("issues.resolve")}
                icon={<CheckCircle2 className="size-4" />}
                onClick={() => onApplyStatus("resolved")}
              />
              <RowAction
                title={t("issues.ignore")}
                icon={<VolumeX className="size-4" />}
                onClick={() => onApplyStatus("ignored")}
              />
            </>
          ) : (
            <RowAction
              title={t("issues.reopen")}
              icon={<RotateCcw className="size-4" />}
              onClick={() => onApplyStatus("open")}
            />
          )}
        </div>
      )}
    </li>
  );
}

function RowAction({
  title,
  icon,
  onClick,
}: {
  title: string;
  icon: React.ReactNode;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      title={title}
      aria-label={title}
      onClick={onClick}
      className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
    >
      {icon}
    </button>
  );
}
