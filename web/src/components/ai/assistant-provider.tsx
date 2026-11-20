import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
import { assistantQuery, type AiSignal } from "@/lib/api/ai";
import { ApiError } from "@/lib/api/client";
import { useAuth } from "@/lib/auth/auth-provider";
import { handleSessionExpiry } from "@/lib/auth/session-expiry";
import { useProject } from "@/lib/project/project-provider";
import dayjs from "@/lib/dayjs";
import {
  ASSISTANT_MAX_TURNS,
  assistantHistoryKey,
  clearAssistantHistory,
  loadAssistantHistory,
  saveAssistantHistory,
  type AssistantTurn,
} from "./assistant-storage";

export type { AssistantTurn } from "./assistant-storage";

/**
 * Global assistant conversation state. Lifted above the panel (which the Radix
 * Sheet unmounts when closed) so multi-turn history survives open/close within a
 * session; persisted per (user, project) so switching projects or accounts never
 * mixes conversation turns or leaks query results, and cleared on sign-out.
 */

interface AssistantContextValue {
  open: boolean;
  setOpen: (open: boolean) => void;
  turns: AssistantTurn[];
  send: (question: string, signal: AiSignal, timeRange: string) => void;
  clear: () => void;
}

const AssistantContext = createContext<AssistantContextValue | null>(null);

interface ScopedTurnsState {
  scopeKey: string | null;
  turns: AssistantTurn[];
}

export function AssistantProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const { currentSlug } = useProject();
  const scopeKey = assistantHistoryKey(user, currentSlug);

  const [open, setOpen] = useState(false);
  const [state, setState] = useState<ScopedTurnsState>(() => ({
    scopeKey,
    turns: loadAssistantHistory(scopeKey),
  }));
  const seq = useRef(0);
  const activeScopeRef = useRef<string | null>(scopeKey);
  const inFlightRef = useRef(new Map<string, AbortController>());

  const abortAllInFlight = useCallback(() => {
    for (const controller of inFlightRef.current.values()) {
      controller.abort();
    }
    inFlightRef.current.clear();
  }, []);

  // Cancel in-flight requests and switch to the new (user, project) history.
  useEffect(() => {
    if (activeScopeRef.current === scopeKey) return;
    activeScopeRef.current = scopeKey;
    abortAllInFlight();
    setState({
      scopeKey,
      turns: loadAssistantHistory(scopeKey),
    });
  }, [scopeKey, abortAllInFlight]);

  useEffect(() => abortAllInFlight, [abortAllInFlight]);

  useEffect(() => {
    if (state.scopeKey !== scopeKey) return;
    saveAssistantHistory(state.scopeKey, state.turns);
  }, [scopeKey, state.scopeKey, state.turns]);

  const patchTurn = useCallback((
    expectedScope: string | null,
    id: string,
    patch: Partial<AssistantTurn>,
  ) => {
    setState((prev) => {
      if (prev.scopeKey !== expectedScope) return prev;
      return {
        ...prev,
        turns: prev.turns.map((t) => (t.id === id ? { ...t, ...patch } : t)),
      };
    });
  }, []);

  const send = useCallback(
    (question: string, signal: AiSignal, timeRange: string) => {
      const q = question.trim();
      if (!q) return;
      const requestScope = activeScopeRef.current;
      const requestProject = currentSlug;
      const id = `${dayjs().valueOf()}-${seq.current++}`;
      const turn: AssistantTurn = {
        id,
        question: q,
        requestedSignal: signal,
        status: "pending",
      };
      setState((prev) => {
        if (prev.scopeKey !== requestScope) return prev;
        return {
          ...prev,
          turns: [...prev.turns, turn].slice(-ASSISTANT_MAX_TURNS),
        };
      });

      const controller = new AbortController();
      inFlightRef.current.set(id, controller);

      assistantQuery(
        { question: q, signal, time_range: timeRange },
        controller.signal,
        requestProject,
      )
        .then((answer) => {
          if (controller.signal.aborted || activeScopeRef.current !== requestScope) return;
          patchTurn(requestScope, id, { status: "done", answer });
        })
        .catch((e) => {
          if (
            controller.signal.aborted
            || (e instanceof DOMException && e.name === "AbortError")
            || activeScopeRef.current !== requestScope
          ) {
            return;
          }
          handleSessionExpiry(queryClient, e);
          const status = e instanceof ApiError ? e.status : 0;
          const message = e instanceof Error ? e.message : String(e);
          patchTurn(requestScope, id, { status: "error", errorStatus: status, errorMessage: message });
        })
        .finally(() => {
          if (inFlightRef.current.get(id) === controller) {
            inFlightRef.current.delete(id);
          }
        });
    },
    [currentSlug, patchTurn, queryClient],
  );

  const clear = useCallback(() => {
    abortAllInFlight();
    clearAssistantHistory(activeScopeRef.current);
    setState({ scopeKey: activeScopeRef.current, turns: [] });
  }, [abortAllInFlight]);

  const turns = state.scopeKey === scopeKey ? state.turns : [];

  return (
    <AssistantContext.Provider value={{ open, setOpen, turns, send, clear }}>
      {children}
    </AssistantContext.Provider>
  );
}

// oxlint-disable-next-line react/only-export-components
export function useAssistant(): AssistantContextValue {
  const ctx = useContext(AssistantContext);
  if (!ctx) throw new Error("useAssistant must be used within AssistantProvider");
  return ctx;
}
