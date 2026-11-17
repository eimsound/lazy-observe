import {
  LayoutDashboard,
  LineChart,
  ScrollText,
  Waypoints,
  Bug,
  MonitorPlay,
  Gauge,
  Flame,
  Server,
  Target,
  BellRing,
  ListChecks,
  BellOff,
  Send,
  FolderKanban,
  Users,
  Settings,
  Sparkles,
  Bot,
  History,
  Stethoscope,
  type LucideIcon,
} from "lucide-react";

/**
 * Primary navigation model (GitLab-style grouped sidebar). Adding a section =
 * one entry here + a route in src/router.tsx. Items may be gated by role
 * (`adminOnly`) — the sidebar hides those the current user can't reach.
 */

export interface NavItem {
  /** Route path. */
  to: string;
  /** i18n key under `nav.*`. */
  labelKey: string;
  icon: LucideIcon;
  /** Not yet implemented — shows a placeholder page + a muted style. */
  placeholder?: boolean;
  /** Only visible to admins (e.g. user management). */
  adminOnly?: boolean;
}

export interface NavGroup {
  /** i18n key under `nav.*`. */
  labelKey: string;
  items: NavItem[];
}

export const NAV_GROUPS: NavGroup[] = [
  {
    labelKey: "nav.groupMonitor",
    items: [
      { to: "/", labelKey: "nav.overview", icon: LayoutDashboard },
      { to: "/metrics", labelKey: "nav.metrics", icon: LineChart },
      { to: "/logs", labelKey: "nav.logs", icon: ScrollText },
      { to: "/traces", labelKey: "nav.traces", icon: Waypoints },
      { to: "/issues", labelKey: "nav.issues", icon: Bug },
      { to: "/replays", labelKey: "nav.replays", icon: MonitorPlay },
      { to: "/performance", labelKey: "nav.performance", icon: Gauge },
      { to: "/llm", labelKey: "nav.llm", icon: Bot },
      { to: "/profiling", labelKey: "nav.profiling", icon: Flame },
    ],
  },
  {
    labelKey: "nav.groupInfra",
    items: [
      { to: "/infrastructure/hosts", labelKey: "nav.hosts", icon: Server },
      { to: "/infrastructure/scrape-targets", labelKey: "nav.scrapeTargets", icon: Target },
    ],
  },
  {
    labelKey: "nav.groupAlerts",
    items: [
      { to: "/alerts", labelKey: "nav.alertEvents", icon: BellRing },
      { to: "/alerts/rules", labelKey: "nav.alertRules", icon: ListChecks },
      { to: "/alerts/silences", labelKey: "nav.silences", icon: BellOff },
      { to: "/alerts/channels", labelKey: "nav.channels", icon: Send },
      { to: "/alerts/notifications", labelKey: "nav.notifications", icon: History },
    ],
  },
  {
    labelKey: "nav.groupManage",
    items: [
      { to: "/settings/projects", labelKey: "nav.projects", icon: FolderKanban },
      { to: "/settings/users", labelKey: "nav.users", icon: Users, adminOnly: true },
      { to: "/settings/audit", labelKey: "nav.audit", icon: ScrollText, adminOnly: true },
      { to: "/settings/diagnostics", labelKey: "nav.diagnostics", icon: Stethoscope, adminOnly: true },
      { to: "/settings/ai", labelKey: "nav.aiSettings", icon: Sparkles, adminOnly: true },
      { to: "/settings", labelKey: "nav.settings", icon: Settings },
    ],
  },
];

/** Flat lookup for breadcrumb resolution. */
export const NAV_ITEMS: NavItem[] = NAV_GROUPS.flatMap((g) => g.items);

/** Resolve the most specific nav item matching a pathname (longest prefix). */
export function resolveNavItem(pathname: string): NavItem | undefined {
  return NAV_ITEMS.filter((i) =>
    i.to === "/" ? pathname === "/" : pathname === i.to || pathname.startsWith(i.to + "/") || pathname.startsWith(i.to),
  ).sort((a, b) => b.to.length - a.to.length)[0];
}
