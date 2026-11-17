import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { listProjects, type Project } from "@/lib/api/projects";
import { getProjectSlug, setProjectSlug } from "@/lib/settings";
import { switchProjectQueries } from "./project-scope";

/**
 * Selected-project state (session mode). Mounted inside the authed layout so it
 * only loads projects when signed in. The current slug is persisted and sent as
 * `X-LO-Project` on every data read (see client.ts). The selection is per tab.
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
  const queryClient = useQueryClient();
  const { data, isLoading } = useQuery<Project[]>({
    queryKey: PROJECTS_KEY,
    queryFn: ({ signal }) => listProjects(signal),
    staleTime: 60_000,
  });

  const projects = useMemo(() => data ?? [], [data]);
  const [currentSlug, setCurrentSlug] = useState<string>(() => getProjectSlug());

  // Auto-select a project once the list loads (or if the stored one vanished).
  useEffect(() => {
    if (projects.length === 0) return;
    if (!currentSlug || !projects.some((p) => p.slug === currentSlug)) {
      setProjectSlug(projects[0].slug);
      setCurrentSlug(projects[0].slug);
      void switchProjectQueries(queryClient);
    }
  }, [projects, currentSlug, queryClient]);

  const value = useMemo<ProjectContextValue>(() => {
    const current = projects.find((p) => p.slug === currentSlug) ?? null;
    return {
      projects,
      current,
      currentSlug,
      isLoading,
      setCurrent: (slug: string) => {
        if (slug === currentSlug) return;
        setProjectSlug(slug);
        setCurrentSlug(slug);
        // Drop and refetch everything cached under the previous project.
        void switchProjectQueries(queryClient);
      },
    };
  }, [projects, currentSlug, isLoading, queryClient]);

  return <ProjectContext.Provider value={value}>{children}</ProjectContext.Provider>;
}

// eslint-disable-next-line react-refresh/only-export-components
export function useProject(): ProjectContextValue {
  const ctx = useContext(ProjectContext);
  if (!ctx) throw new Error("useProject must be used within ProjectProvider");
  return ctx;
}
