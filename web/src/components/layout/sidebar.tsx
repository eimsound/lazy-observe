import { Link, useRouterState } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Activity, Check, ChevronsUpDown, PanelLeftClose, PanelLeftOpen } from "lucide-react";
import { canAccessNavItem, NAV_GROUPS, resolveNavItem, type NavItem } from "@/config/nav";
import { useSidebar } from "@/hooks/use-sidebar";
import { useAuth } from "@/lib/auth/auth-provider";
import { useProject } from "@/lib/project/project-provider";
import { cn } from "@/lib/utils";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

/**
 * GitLab-style primary navigation sidebar. Fixed to the left; collapses to a
 * 56px icon rail (labels hidden, tooltips on hover). Grouped sections with
 * active-route highlighting; admin-only items are hidden for non-admins. Active
 * state is resolved to a single most-specific item so nested routes (e.g.
 * /alerts/rules) don't also light up their parent (/alerts).
 */
const sidebarItemClassName =
  "group flex h-9 w-full items-center gap-3 rounded-md px-2 text-sm font-medium text-sidebar-foreground/80 transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

export function Sidebar() {
  const { t } = useTranslation();
  const { collapsed, toggle } = useSidebar();
  const { role } = useAuth();
  const { projects, current, setCurrent, isLoading } = useProject();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const activeTo = resolveNavItem(pathname)?.to;

  const visible = (item: NavItem) => canAccessNavItem(item, role);

  return (
    <aside
      className={cn(
        "flex h-full flex-col bg-sidebar text-sidebar-foreground transition-[width] duration-200 ease-in-out",
        collapsed ? "w-14" : "w-60",
      )}
    >
      {/* Brand and project switcher */}
      <div className="flex h-14 items-center px-2">
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button
              type="button"
              className={cn(
                "flex h-11 w-full min-w-0 items-center gap-2 rounded-lg px-1 text-left transition-colors hover:bg-sidebar-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                collapsed && "justify-center px-0",
              )}
              aria-label={t("topbar.switchProject")}
            >
              <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-primary text-primary-foreground">
                <Activity className="size-5" />
              </span>
              {!collapsed && (
                <>
                  <span className="flex min-w-0 flex-1 flex-col leading-tight">
                    <span className="truncate text-sm font-semibold tracking-tight">
                      {t("common.appName")}
                    </span>
                    <span className="truncate text-[11px] font-normal text-sidebar-foreground/60">
                      {isLoading
                        ? t("topbar.loadingProjects")
                        : (current?.name ?? t("topbar.noProjects"))}
                    </span>
                  </span>
                  <ChevronsUpDown className="size-3.5 shrink-0 opacity-50" />
                </>
              )}
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" sideOffset={4} className="min-w-56">
            <DropdownMenuLabel>{t("topbar.switchProject")}</DropdownMenuLabel>
            <DropdownMenuSeparator />
            {projects.length === 0 ? (
              <DropdownMenuItem disabled>{t("topbar.noProjects")}</DropdownMenuItem>
            ) : (
              projects.map((project) => (
                <DropdownMenuItem
                  key={project.id}
                  onSelect={() => setCurrent(project.slug)}
                  className="justify-between"
                >
                  <span className="flex min-w-0 flex-col">
                    <span className="truncate">{project.name}</span>
                    <span className="truncate font-mono text-[11px] text-muted-foreground">
                      {project.slug}
                    </span>
                  </span>
                  {current?.slug === project.slug && <Check className="size-4 shrink-0" />}
                </DropdownMenuItem>
              ))
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {/* Nav groups */}
      <nav className="flex-1 overflow-y-auto px-2 py-3">
        {NAV_GROUPS.map((group) => {
          const items = group.items.filter(visible);
          if (items.length === 0) return null;
          return (
            <div key={group.labelKey} className="mb-4">
              {!collapsed && (
                <div className="px-2 pb-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                  {t(group.labelKey)}
                </div>
              )}
              <ul className="flex flex-col gap-0.5">
                {items.map((item) => {
                  const Icon = item.icon;
                  const isActive = item.to === activeTo;
                  const link = (
                    <Link
                      to={item.to}
                      className={cn(
                        sidebarItemClassName,
                        collapsed && "justify-center",
                        isActive &&
                          "bg-sidebar-accent text-sidebar-accent-foreground",
                      )}
                    >
                      <Icon className="size-4 shrink-0" />
                      {!collapsed && <span className="truncate">{t(item.labelKey)}</span>}
                      {!collapsed && item.placeholder && (
                        <span className="ml-auto size-1.5 rounded-full bg-muted-foreground/40" />
                      )}
                    </Link>
                  );

                  return (
                    <li key={item.to}>
                      {collapsed ? (
                        <Tooltip delayDuration={0}>
                          <TooltipTrigger asChild>{link}</TooltipTrigger>
                          <TooltipContent side="right">{t(item.labelKey)}</TooltipContent>
                        </Tooltip>
                      ) : (
                        link
                      )}
                    </li>
                  );
                })}
              </ul>
            </div>
          );
        })}
      </nav>

      {/* Collapse toggle */}
      <div className="border-t border-sidebar-border p-2">
        <button
          type="button"
          onClick={toggle}
          className={cn(sidebarItemClassName, collapsed ? "justify-center" : "justify-start")}
          aria-label={collapsed ? t("nav.expand") : t("nav.collapse")}
        >
          {collapsed ? (
            <PanelLeftOpen className="size-4" />
          ) : (
            <>
              <PanelLeftClose className="size-4" />
              <span>{t("nav.collapse")}</span>
            </>
          )}
        </button>
      </div>
    </aside>
  );
}
