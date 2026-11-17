export type QueryViewState = "pending" | "error" | "empty" | "stale" | "ready";

export interface QueryViewStateInput {
  hasData: boolean;
  isLoading: boolean;
  isError: boolean;
  isEmpty: boolean;
}

export function queryViewState({
  hasData,
  isLoading,
  isError,
  isEmpty,
}: QueryViewStateInput): QueryViewState {
  if (isLoading && !hasData) return "pending";
  if (isError) return hasData ? "stale" : "error";
  if (!hasData || isEmpty) return "empty";
  return "ready";
}
