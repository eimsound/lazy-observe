import {
  useQueries,
  useQuery,
  type QueryFunctionContext,
  type UseQueryOptions,
  type UseQueryResult,
} from "@tanstack/react-query";
import { useProject } from "./project-provider";
import { projectQueryKey } from "./project-scope";

export type ProjectQueryKey = readonly [string, ...unknown[]];

export interface ProjectQueryFunctionContext<
  TQueryKey extends ProjectQueryKey = ProjectQueryKey,
> {
  signal: AbortSignal;
  project: string;
  queryKey: readonly [TQueryKey[0], string, ...unknown[]];
  meta: QueryFunctionContext["meta"];
}

export type UseProjectQueryOptions<
  TQueryFnData = unknown,
  TError = Error,
  TData = TQueryFnData,
  TQueryKey extends ProjectQueryKey = ProjectQueryKey,
> = Omit<
  UseQueryOptions<
    TQueryFnData,
    TError,
    TData,
    readonly [TQueryKey[0], string, ...unknown[]]
  >,
  "queryKey" | "queryFn" | "enabled"
> & {
  queryKey: TQueryKey;
  queryFn: (context: ProjectQueryFunctionContext<TQueryKey>) => Promise<TQueryFnData>;
  enabled?: boolean;
  /**
   * Explicit project slug override. Defaults to the reactive `currentSlug`
   * from `useProject()`.
   */
  project?: string;
  /**
   * Allow executing even when `currentSlug` is empty (only for pages that
   * explicitly handle zero-project workspaces). Defaults to `false`.
   */
  allowEmptyProject?: boolean;
};

/**
 * Project-scoped wrapper around TanStack Query's `useQuery`.
 *
 * Automatically embeds the active project slug into `queryKey` (keeping the
 * domain root at index 0) and passes `project` explicitly into `queryFn`, so
 * UI callers never rely on implicit `getProjectSlug()` reads and switching
 * projects cannot serve stale cross-project cache entries.
 */
export function useProjectQuery<
  TQueryFnData = unknown,
  TError = Error,
  TData = TQueryFnData,
  TQueryKey extends ProjectQueryKey = ProjectQueryKey,
>(
  options: UseProjectQueryOptions<TQueryFnData, TError, TData, TQueryKey>,
): UseQueryResult<TData, TError> {
  const { currentSlug } = useProject();
  const {
    queryKey,
    queryFn,
    enabled = true,
    project: projectOverride,
    allowEmptyProject = false,
    ...rest
  } = options;
  const project = projectOverride ?? currentSlug;
  const scopedKey = projectQueryKey(project, queryKey);

  return useQuery<TQueryFnData, TError, TData, typeof scopedKey>({
    ...rest,
    queryKey: scopedKey,
    enabled: (allowEmptyProject || Boolean(project)) && enabled,
    queryFn: ({ signal, queryKey: key, meta }) =>
      queryFn({ signal, project, queryKey: key, meta }),
  });
}

/**
 * Project-scoped wrapper around TanStack Query's `useQueries` for fan-out
 * panels (such as host metric charts).
 */
export function useProjectQueries<
  TQueryFnData = unknown,
  TError = Error,
  TData = TQueryFnData,
  TQueryKey extends ProjectQueryKey = ProjectQueryKey,
>(options: {
  queries: ReadonlyArray<UseProjectQueryOptions<TQueryFnData, TError, TData, TQueryKey>>;
}): UseQueryResult<TData, TError>[] {
  const { currentSlug } = useProject();

  return useQueries({
    queries: options.queries.map((item) => {
      const {
        queryKey,
        queryFn,
        enabled = true,
        project: projectOverride,
        allowEmptyProject = false,
        ...rest
      } = item;
      const project = projectOverride ?? currentSlug;
      const scopedKey = projectQueryKey(project, queryKey);
      return {
        ...rest,
        queryKey: scopedKey,
        enabled: (allowEmptyProject || Boolean(project)) && enabled,
        queryFn: ({ signal, queryKey: key, meta }: QueryFunctionContext<typeof scopedKey>) =>
          queryFn({ signal, project, queryKey: key, meta }),
      };
    }),
  });
}
