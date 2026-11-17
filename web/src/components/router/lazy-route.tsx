import { Suspense, lazy, type ReactNode } from "react";
import { RouteModuleBoundary, RouteModuleFallback } from "./route-module-boundary";

type ModuleRecord = Record<string, unknown>;

export function lazyRouteComponent<TModule extends ModuleRecord, TExport extends keyof TModule>(
  load: () => Promise<TModule>,
  exportName: TExport,
): () => ReactNode {
  const LazyComponent = lazy(async () => {
    const mod = await load();
    const resolved = mod[exportName];
    if (typeof resolved !== "function") {
      throw new Error(`Route export "${String(exportName)}" is not a React component`);
    }
    return { default: resolved as () => ReactNode };
  });

  return function LazyRouteComponent() {
    return (
      <RouteModuleBoundary>
        <Suspense fallback={<RouteModuleFallback />}>
          <LazyComponent />
        </Suspense>
      </RouteModuleBoundary>
    );
  };
}
