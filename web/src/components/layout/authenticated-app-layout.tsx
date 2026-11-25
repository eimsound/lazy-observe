import { Fragment, useEffect } from "react";
import { Outlet, useRouterState } from "@tanstack/react-router";
import { canAccessRoute } from "@/lib/auth/permissions";
import { ProjectProvider, useProject } from "@/lib/project/project-provider";
import { routeProjectScopeKey } from "@/lib/project/route-scope";
import { initWebSelfMonitoring } from "@/lib/self-monitoring";
import { AppShell } from "./app-shell";
import { RouteAccessGuard } from "./route-access-guard";

function ScopedOutlet() {
  const { currentSlug } = useProject();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const scopeKey = routeProjectScopeKey(pathname, currentSlug);

  return (
    <RouteAccessGuard>
      <Fragment key={scopeKey}>
        <Outlet />
      </Fragment>
    </RouteAccessGuard>
  );
}

export function AuthenticatedAppLayout() {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const blockReplay = !canAccessRoute(pathname, "viewer");

  useEffect(() => {
    void initWebSelfMonitoring();
  }, []);

  return (
    <ProjectProvider>
      <AppShell blockReplay={blockReplay}>
        <ScopedOutlet />
      </AppShell>
    </ProjectProvider>
  );
}
