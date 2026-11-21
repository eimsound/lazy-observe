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
  ShieldBan,
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

import type { Role } from "@/lib/api/auth";
import {
  hasCapability,
  hasMinimumRole,
  minRoleForCapability,
  type Capability,
} from "@/lib/auth/permissions";

/**
 * Primary navigation model (GitLab-style grouped sidebar). Adding a section =
 * one entry here + a route in src/router.tsx. Items may be gated by
 * capability (`capability`) and/or role (`minRole`) — the sidebar, command
 * palette, and route guard all share the same RBAC capability matrix.
 */

export interface NavItem {
  /** Route path. */
  to: string;
  /** i18n key under `nav.*`. */
  labelKey: string;
  icon: LucideIcon;
  /** Not yet implemented — shows a placeholder page + a muted style. */
  placeholder?: boolean;
  /** Fine-grained capability required to access this route (matches backend RBAC). */
  capability?: Capability;
  /** Minimum role derived from capability or set explicitly. */
  minRole?: Role;
}

export interface NavGroup {
  /** i18n key under `nav.*`. */
  labelKey: string;
  items: NavItem[];
}

function gated(
  item: Omit<NavItem, "minRole"> & { capability: Capability },
): NavItem {
  return {
    ...item,
    minRole: minRoleForCapability(item.capability),
  };
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
      gated({ to: "/alerts/rules", labelKey: "nav.alertRules", icon: ListChecks, capability: "alerts:read" }),
      gated({ to: "/alerts/silences", labelKey: "nav.silences", icon: BellOff, capability: "alerts:read" }),
      gated({ to: "/alerts/inhibitions", labelKey: "nav.inhibitions", icon: ShieldBan, capability: "alerts:read" }),
      gated({ to: "/alerts/channels", labelKey: "nav.channels", icon: Send, capability: "channels:write" }),
      gated({ to: "/alerts/notifications", labelKey: "nav.notifications", icon: History, capability: "alerts:read" }),
    ],
  },
  {
    labelKey: "nav.groupManage",
    items: [
      { to: "/settings/projects", labelKey: "nav.projects", icon: FolderKanban },
      gated({ to: "/settings/users", labelKey: "nav.users", icon: Users, capability: "users:manage" }),
      gated({ to: "/settings/audit", labelKey: "nav.audit", icon: ScrollText, capability: "audit:read" }),
      gated({ to: "/settings/diagnostics", labelKey: "nav.diagnostics", icon: Stethoscope, capability: "diagnostics:read" }),
      gated({ to: "/settings/ai", labelKey: "nav.aiSettings", icon: Sparkles, capability: "ai:manage" }),
      { to: "/settings", labelKey: "nav.settings", icon: Settings },
    ],
  },
];

/** Whether a role can access a navigation entry (shared by sidebar & command palette). */
export function canAccessNavItem(
  item: Pick<NavItem, "capability" | "minRole">,
  role: Role | null,
): boolean {
  if (item.capability && !hasCapability(role, item.capability)) {
    return false;
  }
  return hasMinimumRole(role, item.minRole);
}

/** Flat lookup for breadcrumb resolution. */
export const NAV_ITEMS: NavItem[] = NAV_GROUPS.flatMap((g) => g.items);

/** Resolve the most specific nav item matching a pathname (longest prefix). */
export function resolveNavItem(pathname: string): NavItem | undefined {
  return NAV_ITEMS.filter((i) =>
    i.to === "/" ? pathname === "/" : pathname === i.to || pathname.startsWith(i.to + "/") || pathname.startsWith(i.to),
  ).sort((a, b) => b.to.length - a.to.length)[0];
}
