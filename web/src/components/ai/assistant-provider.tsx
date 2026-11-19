import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { assistantQuery, type AiSignal, type AssistantAnswer } from "@/lib/api/ai";
import { ApiError } from "@/lib/api/client";
import dayjs from "@/lib/dayjs";

/**
 * Global assistant conversation state. Lifted above the panel (which the Radix
 * Sheet unmounts when closed) so multi-turn history survives open/close within a
 * session; the most recent turns are also persisted to localStorage so they
 * survive reloads. Opened from the top bar; rendered once inside the app shell.
 */

export interface AssistantTurn {
  id: string;
  question: string;
  /** The signal the user requested for this turn (auto/metrics/logs). */
  requestedSignal: AiSignal;
  status: "pending" | "done" | "error";
  answer?: AssistantAnswer;
  /** HTTP status of a failed request (429 -> rate limited). */
  errorStatus?: number;
  errorMessage?: string;
}

interface AssistantContextValue {
  open: boolean;
  setOpen: (open: boolean) => void;
  turns: AssistantTurn[];
  send: (question: string, signal: AiSignal, timeRange: string) => void;
  clear: () => void;
}

const HISTORY_KEY = "lo-assistant-history";
const MAX_TURNS = 20;

const AssistantContext = createContext<AssistantContextValue | null>(null);

/** Only completed/failed turns are worth persisting (drop in-flight ones). */
function loadHistory(): AssistantTurn[] {
  try {
    const raw = localStorage.getItem(HISTORY_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as AssistantTurn[];
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((t) => t.status !== "pending").slice(-MAX_TURNS);
  } catch {
    return [];
  }
}

function saveHistory(turns: AssistantTurn[]): void {
  try {
    const persistable = turns.filter((t) => t.status !== "pending").slice(-MAX_TURNS);
    localStorage.setItem(HISTORY_KEY, JSON.stringify(persistable));
  } catch {
    /* ignore (private mode / disabled storage) */
  }
}

export function AssistantProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const [turns, setTurns] = useState<AssistantTurn[]>(() => loadHistory());
  const seq = useRef(0);

  useEffect(() => {
    saveHistory(turns);
  }, [turns]);

  const patchTurn = useCallback((id: string, patch: Partial<AssistantTurn>) => {
    setTurns((prev) => prev.map((t) => (t.id === id ? { ...t, ...patch } : t)));
  }, []);

  const send = useCallback(
    (question: string, signal: AiSignal, timeRange: string) => {
      const q = question.trim();
      if (!q) return;
      const id = `${dayjs().valueOf()}-${seq.current++}`;
      const turn: AssistantTurn = {
        id,
        question: q,
        requestedSignal: signal,
        status: "pending",
      };
      setTurns((prev) => [...prev, turn].slice(-MAX_TURNS));

      assistantQuery({ question: q, signal, time_range: timeRange })
        .then((answer) => patchTurn(id, { status: "done", answer }))
        .catch((e) => {
          const status = e instanceof ApiError ? e.status : 0;
          const message = e instanceof Error ? e.message : String(e);
          patchTurn(id, { status: "error", errorStatus: status, errorMessage: message });
        });
    },
    [patchTurn],
  );

  const clear = useCallback(() => setTurns([]), []);

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
