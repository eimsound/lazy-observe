import type { ReactNode } from "react";
import { useRouterState } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { ShieldAlert } from "lucide-react";
import { useAuth } from "@/lib/auth/auth-provider";
import { canAccessRoute, resolveRouteAccess } from "@/lib/auth/permissions";
import { PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { Card, CardContent } from "@/components/ui/card";

/**
 * Centralized route-level RBAC guard. Evaluates the current pathname against
 * `ROUTE_ACCESS_RULES` before the target page mounts, preventing unauthorized
 * routes from rendering stale shells or issuing forbidden queries.
 */
export function RouteAccessGuard({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const { role } = useAuth();
  const pathname = useRouterState({ select: (s) => s.location.pathname });

  if (!canAccessRoute(pathname, role)) {
    const rule = resolveRouteAccess(pathname);
    return (
      <PageContainer>
        <Card>
          <CardContent className="py-12">
            <EmptyState
              icon={<ShieldAlert className="size-6" />}
              title={t(rule?.forbiddenTitleKey ?? "common.forbiddenTitle")}
              desc={t(rule?.forbiddenDescKey ?? "common.forbiddenDesc")}
            />
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  return <>{children}</>;
}
