import { useRouterState } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { ChevronRight } from "lucide-react";
import { resolveNavItem } from "@/config/nav";

/**
 * Breadcrumb trail derived from the active route. M0 has a flat route table, so
 * this resolves "App / <section>". Extend with nested crumbs as sub-routes land.
 */
export function Breadcrumbs() {
  const { t } = useTranslation();
  const pathname = useRouterState({ select: (s) => s.location.pathname });

  const current = resolveNavItem(pathname);

  // Sub-route leaf crumb (e.g. a specific trace id under /traces/:id).
  const traceMatch = pathname.match(/^\/traces\/(.+)$/);
  const leaf = traceMatch ? traceMatch[1].slice(0, 12) : null;

  return (
    <nav aria-label={t("common.breadcrumb")} className="flex items-center gap-1.5 text-sm">
      <span className="text-muted-foreground">{t("common.appName")}</span>
      {current && (
        <>
          <ChevronRight className="size-3.5 text-muted-foreground/60" />
          <span
            className={
              leaf ? "text-muted-foreground" : "font-medium text-foreground"
            }
          >
            {t(current.labelKey)}
          </span>
        </>
      )}
      {leaf && (
        <>
          <ChevronRight className="size-3.5 text-muted-foreground/60" />
          <span className="font-mono font-medium text-foreground">{leaf}</span>
        </>
      )}
    </nav>
  );
}
