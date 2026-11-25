import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRouter, useRouterState } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { AlertCircle, Loader2 } from "lucide-react";
import { listProjects, type Project } from "@/lib/api/projects";
import { getProjectSlug, setProjectSlug } from "@/lib/settings";
import { Button } from "@/components/ui/button";
import {
  readProjectLinkParam,
  resolveProjectSelection,
  stripProjectLinkParam,
  switchProjectQueries,
  type ProjectSelectionResolution,
} from "./project-scope";

/**
 * Selected-project state (session mode). Mounted inside the authed layout so it
 * only loads projects when signed in. The current slug is persisted and sent as
 * `X-LO-Project` on every data read (see client.ts). The selection is per tab.
 *
 * Child routes are deferred until `/api/projects` resolves and the active slug
 * is validated against that list so a stale `lo-project` (deleted or renamed)
 * never fires data requests that bounce back as 401/404 in a sign-out loop.
 * Switching projects resets project-scoped cached queries (switchProjectQueries)
 * so no page renders results fetched for the previous project.
 */

interface ProjectContextValue {
  projects: Project[];
  current: Project | null;
  currentSlug: string;
  isLoading: boolean;
  setCurrent: (slug: string) => void;
}

const ProjectContext = createContext<ProjectContextValue | null>(null);

const PROJECTS_KEY = ["projects"] as const;

export function ProjectProvider({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const projectsQuery = useQuery<Project[]>({
    queryKey: PROJECTS_KEY,
    queryFn: ({ signal }) => listProjects(signal),
    staleTime: 60_000,
    retry: false,
  });

  const projects = useMemo(() => projectsQuery.data ?? [], [projectsQuery.data]);
  const [currentSlug, setCurrentSlug] = useState<string>(() => getProjectSlug());

  // Console links (alert notifications) name their project in `?project=`.
  // Read the raw search string: route `validateSearch` whitelists drop it.
  const router = useRouter();
  const location = useRouterState({
    select: (s) => ({ pathname: s.location.pathname, searchStr: s.location.searchStr, hash: s.location.hash }),
  });
  const linkSlug = readProjectLinkParam(location.searchStr);

  const selection = useMemo<ProjectSelectionResolution>(
    () => (projectsQuery.isSuccess
      ? resolveProjectSelection(projects, currentSlug, linkSlug)
      : { ready: false, resolvedSlug: currentSlug, changed: false }),
    [projectsQuery.isSuccess, projects, currentSlug, linkSlug],
  );

  // Once the linked project is active, drop `?project=` (keep other params) so
  // a later manual switch or reload is not pulled back to the linked project.
  useEffect(() => {
    if (!linkSlug || !selection.ready) return;
    const hash = location.hash ? `#${location.hash}` : "";
    router.history.replace(`${location.pathname}${stripProjectLinkParam(location.searchStr)}${hash}`);
  }, [linkSlug, selection.ready, location.pathname, location.searchStr, location.hash, router]);

  // Auto-select a valid project once the list loads (or if the stored one vanished).
  useEffect(() => {
    if (!projectsQuery.isSuccess || !selection.changed) return;
    let cancelled = false;
    setProjectSlug(selection.resolvedSlug);
    void switchProjectQueries(queryClient).then(() => {
      if (!cancelled) {
        setCurrentSlug(selection.resolvedSlug);
      }
    });
    return () => {
      cancelled = true;
    };
  }, [projectsQuery.isSuccess, selection.changed, selection.resolvedSlug, queryClient]);

  const value = useMemo<ProjectContextValue>(() => {
    const current = projects.find((p) => p.slug === currentSlug) ?? null;
    return {
      projects,
      current,
      currentSlug,
      isLoading: projectsQuery.isLoading,
      setCurrent: (slug: string) => {
        if (slug === currentSlug) return;
        if (!projects.some((p) => p.slug === slug)) return;
        setProjectSlug(slug);
        setCurrentSlug(slug);
        // Cancel in-flight queries and drop any legacy unkeyed results cached
        // under the previous project.
        void switchProjectQueries(queryClient);
      },
    };
  }, [projects, currentSlug, projectsQuery.isLoading, queryClient]);

  if (projectsQuery.isError) {
    return (
      <div className="flex h-screen w-full items-center justify-center bg-background px-4 text-foreground">
        <div className="flex max-w-sm flex-col items-center gap-3 text-center" role="alert">
          <AlertCircle className="size-6 text-destructive" />
          <div className="text-sm font-medium">{t("common.loadFailed")}</div>
          <p className="text-xs text-muted-foreground">{t("common.queryErrorDesc")}</p>
          <Button variant="outline" size="sm" onClick={() => void projectsQuery.refetch()}>
            {t("common.retry")}
          </Button>
        </div>
      </div>
    );
  }

  if (selection.linkUnavailable) {
    const fallbackSlug = resolveProjectSelection(projects, currentSlug).resolvedSlug;
    const fallback = projects.find((p) => p.slug === fallbackSlug);
    return (
      <div className="flex h-screen w-full items-center justify-center bg-background px-4 text-foreground">
        <div className="flex max-w-sm flex-col items-center gap-3 text-center" role="alert">
          <AlertCircle className="size-6 text-muted-foreground" />
          <div className="break-all text-sm font-medium">
            {t("topbar.linkProjectUnavailable", { slug: linkSlug })}
          </div>
          <p className="text-xs text-muted-foreground">{t("topbar.linkProjectUnavailableDesc")}</p>
          {/* Go to "/" rather than dropping the param in place: staying on
              /issues/42 would open another project's Issue #42. */}
          <Button variant="outline" size="sm" onClick={() => void router.navigate({ to: "/", replace: true })}>
            {fallback
              ? t("topbar.openProject", { name: fallback.name || fallback.slug })
              : t("topbar.returnToConsole")}
          </Button>
        </div>
      </div>
    );
  }

  if (!selection.ready) {
    return (
      <div className="flex h-screen w-full items-center justify-center bg-background text-muted-foreground">
        <div className="flex items-center gap-2 text-sm">
          <Loader2 className="size-4 animate-spin" />
          {t("topbar.loadingProjects")}
        </div>
      </div>
    );
  }

  return <ProjectContext.Provider value={value}>{children}</ProjectContext.Provider>;
}

// oxlint-disable-next-line react/only-export-components
export function useProject(): ProjectContextValue {
  const ctx = useContext(ProjectContext);
  if (!ctx) throw new Error("useProject must be used within ProjectProvider");
  return ctx;
}
