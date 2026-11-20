import {
  useMutation,
  useQueryClient,
  type QueryKey,
  type UseMutationResult,
} from "@tanstack/react-query";
import { formatErrorMessage } from "@/lib/api/errors";

/**
 * Pure optimistic cache helper: applies `updater` only when cached data exists.
 */
export function applyOptimisticData<TData>(
  previous: TData | undefined,
  updater: (data: TData) => TData,
): TData | undefined {
  if (previous === undefined) return previous;
  return updater(previous);
}

export interface UseOptimisticListOptions<TData, TVariables, TResult = unknown> {
  /** Exact query key whose cached list/envelope is optimistically updated. */
  queryKey: QueryKey;
  /** Broader prefix key invalidated on settle (defaults to `queryKey`). */
  invalidateKey?: QueryKey;
  /** Performs the backend mutation. */
  mutationFn: (variables: TVariables) => Promise<TResult>;
  /** Produces the next cached value synchronously before the request settles. */
  optimisticUpdate: (previous: TData, variables: TVariables) => TData;
  /** Optional error callback (called with `null` on mutate start and a formatted message on error). */
  onErrorMessage?: (message: string | null) => void;
  /** Fallback error message when the error lacks a human-readable message. */
  fallbackError: string;
}

/**
 * Shared optimistic list mutation hook: snapshots cached data, applies an
 * immediate optimistic update, rolls back cleanly on error while surfacing a
 * formatted message, and invalidates the backing query prefix on settle.
 */
export function useOptimisticList<TData, TVariables, TResult = unknown>({
  queryKey,
  invalidateKey,
  mutationFn,
  optimisticUpdate,
  onErrorMessage,
  fallbackError,
}: UseOptimisticListOptions<TData, TVariables, TResult>): UseMutationResult<
  TResult,
  unknown,
  TVariables,
  { previous: TData | undefined }
> {
  const queryClient = useQueryClient();

  return useMutation<TResult, unknown, TVariables, { previous: TData | undefined }>({
    mutationFn,
    onMutate: (variables) => {
      onErrorMessage?.(null);
      const previous = queryClient.getQueryData<TData>(queryKey);
      queryClient.setQueryData<TData>(queryKey, (old) =>
        applyOptimisticData(old, (data) => optimisticUpdate(data, variables)),
      );
      return { previous };
    },
    onError: (err, _variables, context) => {
      if (context?.previous !== undefined) {
        queryClient.setQueryData<TData>(queryKey, context.previous);
      }
      onErrorMessage?.(formatErrorMessage(err, fallbackError));
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: invalidateKey ?? queryKey });
    },
  });
}
