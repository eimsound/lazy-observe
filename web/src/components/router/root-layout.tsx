import { useEffect } from "react";
import { Outlet, useRouterState } from "@tanstack/react-router";
import {
  normalizeSelfMonitoringRoutePage,
  trackSelfMonitoringRoute,
} from "@/lib/self-monitoring";

export function RootLayout() {
  const { matches, pathname } = useRouterState({
    select: (s) => ({
      matches: s.matches,
      pathname: s.location.pathname,
    }),
  });
  useEffect(() => {
    trackSelfMonitoringRoute(normalizeSelfMonitoringRoutePage(matches, pathname));
  }, [matches, pathname]);
  return <Outlet />;
}
