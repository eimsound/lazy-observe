import { Fragment } from "react";
import { Outlet, useRouterState } from "@tanstack/react-router";
import { ProjectProvider, useProject } from "@/lib/project/project-provider";
import { routeProjectScopeKey } from "@/lib/project/route-scope";
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
  return (
    <ProjectProvider>
      <AppShell>
        <ScopedOutlet />
      </AppShell>
    </ProjectProvider>
  );
}
