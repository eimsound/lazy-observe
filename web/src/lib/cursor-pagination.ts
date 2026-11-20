/**
 * Cursor-based pagination history stack for newest-first feeds (audit log,
 * notification history). The stack stores `number | undefined` so page 1
 * (`before === undefined`) is always preserved and reachable via `popCursorPage`.
 */
export interface CursorPaginationState {
  before: number | undefined;
  history: Array<number | undefined>;
}

export const INITIAL_CURSOR_PAGINATION: CursorPaginationState = {
  before: undefined,
  history: [],
};

export function pushCursorPage(
  state: CursorPaginationState,
  nextBefore: number,
): CursorPaginationState {
  return {
    before: nextBefore,
    history: [...state.history, state.before],
  };
}

export function popCursorPage(state: CursorPaginationState): CursorPaginationState {
  if (state.history.length === 0) return state;
  return {
    before: state.history[state.history.length - 1],
    history: state.history.slice(0, -1),
  };
}

export function canGoBackCursor(state: CursorPaginationState): boolean {
  return state.history.length > 0;
}
