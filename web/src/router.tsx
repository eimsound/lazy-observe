import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from "@tanstack/react-router";
import { AppLayout } from "@/components/layout/app-layout";
import { lazyRouteComponent } from "@/components/router/lazy-route";
import { parseTraceSearchURL } from "@/routes/traces-search";
import { parseMetricsSearch } from "@/routes/metrics-search";
import { parseProfilingEvidenceSearch } from "@/routes/trace-evidence-links";

const LoginPage = lazyRouteComponent(() => import("@/routes/login-page"), "LoginPage");
const ChangePasswordPage = lazyRouteComponent(
  () => import("@/routes/change-password-page"),
  "ChangePasswordPage",
);
const OverviewPage = lazyRouteComponent(() => import("@/routes/overview-page"), "OverviewPage");
const MetricsPage = lazyRouteComponent(() => import("@/routes/metrics-page"), "MetricsPage");
const LogsPage = lazyRouteComponent(() => import("@/routes/logs-page"), "LogsPage");
const TracesPage = lazyRouteComponent(() => import("@/routes/traces-page"), "TracesPage");
const TraceDetailPage = lazyRouteComponent(() => import("@/routes/trace-detail-page"), "TraceDetailPage");
const IssuesPage = lazyRouteComponent(() => import("@/routes/issues-page"), "IssuesPage");
const IssueDetailPage = lazyRouteComponent(() => import("@/routes/issue-detail-page"), "IssueDetailPage");
const ReplaysPage = lazyRouteComponent(() => import("@/routes/replays-page"), "ReplaysPage");
const ReplayDetailPage = lazyRouteComponent(
  () => import("@/routes/replay-detail-page"),
  "ReplayDetailPage",
);
const PerformancePage = lazyRouteComponent(
  () => import("@/routes/performance-page"),
  "PerformancePage",
);
const LLMObservabilityPage = lazyRouteComponent(
  () => import("@/routes/llm-observability-page"),
  "LLMObservabilityPage",
);
const ProfilingPage = lazyRouteComponent(() => import("@/routes/profiling-page"), "ProfilingPage");
const HostsPage = lazyRouteComponent(() => import("@/routes/hosts-page"), "HostsPage");
const HostDetailPage = lazyRouteComponent(() => import("@/routes/host-detail-page"), "HostDetailPage");
const ScrapeTargetsPage = lazyRouteComponent(
  () => import("@/routes/scrape-targets-page"),
  "ScrapeTargetsPage",
);
const SettingsPage = lazyRouteComponent(() => import("@/routes/settings-page"), "SettingsPage");
const AiSettingsPage = lazyRouteComponent(() => import("@/routes/ai-settings-page"), "AiSettingsPage");
const ProjectsPage = lazyRouteComponent(() => import("@/routes/projects-page"), "ProjectsPage");
const UsersPage = lazyRouteComponent(() => import("@/routes/users-page"), "UsersPage");
const AuditPage = lazyRouteComponent(() => import("@/routes/audit-page"), "AuditPage");
const DiagnosticsPage = lazyRouteComponent(
  () => import("@/routes/diagnostics-page"),
  "DiagnosticsPage",
);
const AlertEventsPage = lazyRouteComponent(() => import("@/routes/alert-events-page"), "AlertEventsPage");
const AlertRulesPage = lazyRouteComponent(() => import("@/routes/alert-rules-page"), "AlertRulesPage");
const SilencesPage = lazyRouteComponent(() => import("@/routes/silences-page"), "SilencesPage");
const InhibitionRulesPage = lazyRouteComponent(
  () => import("@/routes/inhibition-rules-page"),
  "InhibitionRulesPage",
);
const ChannelsPage = lazyRouteComponent(() => import("@/routes/channels-page"), "ChannelsPage");
const NotificationHistoryPage = lazyRouteComponent(
  () => import("@/routes/notification-history-page"),
  "NotificationHistoryPage",
);

/** Logs page search params (deep-link / cross-page prefill from a trace). */
interface LogsSearch {
  q?: string;
  run?: number;
  range?: string;
  start?: number;
  end?: number;
  view?: string;
  limit?: number;
  filters?: unknown;
  refresh?: number;
  autocomplete?: number;
  hide_logs?: number;
  graph?: string;
  graph_by?: string;
  graph_top?: number;
  graph_step?: string;
  stacked?: number;
  cumulative?: number;
  hide_chart?: number;
  group_by?: string;
  display_fields?: string;
  single_line?: number;
  compact_groups?: number;
  markdown?: number;
  ansi?: number;
  disable_hover?: number;
  table_fields?: string;
  table_widths?: string;
  rows_per_page?: number;
  live_tailing_offset?: number;
}

function parseLogsSearch(search: Record<string, unknown>): LogsSearch {
  const stringValue = (key: string) => (
    typeof search[key] === "string" ? search[key] as string : undefined
  );
  const numberValue = (key: string) => {
    const value = search[key];
    const number = typeof value === "number"
      ? value
      : typeof value === "string" && value.trim() ? Number(value) : Number.NaN;
    return Number.isFinite(number) ? number : undefined;
  };
  const flagValue = (key: string) => {
    const value = search[key];
    if (value === true || value === 1 || value === "1" || value === "true") return 1;
    if (value === false || value === 0 || value === "0" || value === "false") return 0;
    return undefined;
  };
  return {
    q: stringValue("q"),
    run: flagValue("run"),
    range: stringValue("range"),
    start: numberValue("start"),
    end: numberValue("end"),
    view: stringValue("view"),
    limit: numberValue("limit"),
    filters: search.filters,
    refresh: numberValue("refresh"),
    autocomplete: flagValue("autocomplete"),
    hide_logs: flagValue("hide_logs"),
    graph: stringValue("graph"),
    graph_by: stringValue("graph_by"),
    graph_top: numberValue("graph_top"),
    graph_step: stringValue("graph_step"),
    stacked: flagValue("stacked"),
    cumulative: flagValue("cumulative"),
    hide_chart: flagValue("hide_chart"),
    group_by: stringValue("group_by"),
    display_fields: stringValue("display_fields"),
    single_line: flagValue("single_line"),
    compact_groups: flagValue("compact_groups"),
    markdown: flagValue("markdown"),
    ansi: flagValue("ansi"),
    disable_hover: flagValue("disable_hover"),
    table_fields: stringValue("table_fields"),
    table_widths: stringValue("table_widths"),
    rows_per_page: numberValue("rows_per_page"),
    live_tailing_offset: numberValue("live_tailing_offset"),
  };
}

/**
 * Code-based route tree (see web/README.md). The root route is a bare Outlet;
 * `/login` renders standalone, while everything else nests under a pathless
 * layout route (`AppLayout`) that enforces the session guard + project scope.
 */

const rootRoute = createRootRoute({
  component: () => <Outlet />,
});

export const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => ({
    redirect: typeof search.redirect === "string" ? search.redirect : undefined,
  }),
  component: LoginPage,
});

// Forced first-login password change. Standalone like /login: the app shell
// would otherwise issue API calls the backend refuses until the change.
export const changePasswordRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/change-password",
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => ({
    redirect: typeof search.redirect === "string" ? search.redirect : undefined,
  }),
  component: ChangePasswordPage,
});

// Pathless layout route: the authenticated shell wraps all app pages.
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "app",
  component: AppLayout,
});

const overviewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/",
  component: OverviewPage,
});

const metricsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/metrics",
  validateSearch: parseMetricsSearch,
  component: MetricsPage,
});

const logsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/logs",
  validateSearch: parseLogsSearch,
  component: LogsPage,
});

const tracesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/traces",
  validateSearch: parseTraceSearchURL,
  component: TracesPage,
});

const traceDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/traces/$traceID",
  component: TraceDetailPage,
});

const issuesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/issues",
  component: IssuesPage,
});

const issueDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/issues/$issueId",
  validateSearch: (search: Record<string, unknown>): { event?: string } => ({
    event: typeof search.event === "string" ? search.event : undefined,
  }),
  component: IssueDetailPage,
});

const replaysRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/replays",
  component: ReplaysPage,
});

const replayDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/replays/$sessionId",
  component: ReplayDetailPage,
});

const performanceRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/performance",
  component: PerformancePage,
});

const llmObservabilityRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/llm",
  component: LLMObservabilityPage,
});

const profilingRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/profiling",
  validateSearch: parseProfilingEvidenceSearch,
  component: ProfilingPage,
});

const hostsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/infrastructure/hosts",
  component: HostsPage,
});

const hostDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/infrastructure/hosts/$instance",
  component: HostDetailPage,
});

const scrapeTargetsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/infrastructure/scrape-targets",
  component: ScrapeTargetsPage,
});

const alertEventsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/alerts",
  component: AlertEventsPage,
});

const alertRulesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/alerts/rules",
  component: AlertRulesPage,
});

const alertSilencesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/alerts/silences",
  component: SilencesPage,
});

const alertInhibitionsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/alerts/inhibitions",
  component: InhibitionRulesPage,
});

const alertChannelsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/alerts/channels",
  component: ChannelsPage,
});

const alertNotificationsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/alerts/notifications",
  component: NotificationHistoryPage,
});

const projectsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings/projects",
  component: ProjectsPage,
});

const usersRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings/users",
  component: UsersPage,
});

const aiSettingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings/ai",
  component: AiSettingsPage,
});

const auditRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings/audit",
  component: AuditPage,
});

const diagnosticsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings/diagnostics",
  component: DiagnosticsPage,
});

const settingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings",
  component: SettingsPage,
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  changePasswordRoute,
  appRoute.addChildren([
    overviewRoute,
    metricsRoute,
    logsRoute,
    tracesRoute,
    traceDetailRoute,
    issuesRoute,
    issueDetailRoute,
    replaysRoute,
    replayDetailRoute,
    performanceRoute,
    llmObservabilityRoute,
    profilingRoute,
    hostsRoute,
    hostDetailRoute,
    scrapeTargetsRoute,
    alertEventsRoute,
    alertRulesRoute,
    alertSilencesRoute,
    alertInhibitionsRoute,
    alertChannelsRoute,
    alertNotificationsRoute,
    projectsRoute,
    usersRoute,
    aiSettingsRoute,
    auditRoute,
    diagnosticsRoute,
    settingsRoute,
  ]),
]);

export const router = createRouter({
  routeTree,
  defaultPreload: "intent",
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
