import {
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from "react";

const SIDEBAR_WIDTH_STORAGE_KEY = "LOGS_FILTER_SIDEBAR_WIDTH";
export const DEFAULT_SIDEBAR_WIDTH = 320;
export const MIN_SIDEBAR_WIDTH = 250;
const SIDEBAR_KEYBOARD_STEP = 16;

export function parseStoredSidebarWidth(raw: string | null): number | undefined {
  if (!raw) return undefined;
  const width = Number(raw);
  return Number.isFinite(width) && width >= MIN_SIDEBAR_WIDTH ? width : undefined;
}

function readStoredSidebarWidth(): number | undefined {
  if (typeof window === "undefined") return undefined;
  try {
    return parseStoredSidebarWidth(window.localStorage.getItem(SIDEBAR_WIDTH_STORAGE_KEY));
  } catch {
    return undefined;
  }
}

function persistSidebarWidth(width: number | undefined) {
  try {
    if (width === undefined) window.localStorage.removeItem(SIDEBAR_WIDTH_STORAGE_KEY);
    else window.localStorage.setItem(SIDEBAR_WIDTH_STORAGE_KEY, String(Math.round(width)));
  } catch {
    // Storage is optional in privacy-restricted browsers.
  }
}

export function clampSidebarWidth(width: number): number {
  const availableWidth = typeof window === "undefined"
    ? width
    : Math.max(MIN_SIDEBAR_WIDTH, window.innerWidth - MIN_SIDEBAR_WIDTH);
  return Math.min(Math.max(width, MIN_SIDEBAR_WIDTH), availableWidth);
}

export function useResizableSidebar() {
  const [sidebarWidth, setSidebarWidth] = useState<number | undefined>(readStoredSidebarWidth);
  const [isResizing, setIsResizing] = useState(false);
  const sidebarRef = useRef<HTMLElement>(null);
  const currentWidth = useRef(sidebarWidth ?? DEFAULT_SIDEBAR_WIDTH);
  const resizeStart = useRef({ pointerX: 0, width: DEFAULT_SIDEBAR_WIDTH });

  const sidebarStyle = {
    "--logs-filter-sidebar-width": `${sidebarWidth ?? DEFAULT_SIDEBAR_WIDTH}px`,
  } as CSSProperties;

  useEffect(() => {
    const syncStoredWidth = (event: StorageEvent) => {
      if (event.key !== SIDEBAR_WIDTH_STORAGE_KEY) return;
      const next = parseStoredSidebarWidth(event.newValue);
      currentWidth.current = next ?? DEFAULT_SIDEBAR_WIDTH;
      setSidebarWidth(next);
    };
    window.addEventListener("storage", syncStoredWidth);
    return () => window.removeEventListener("storage", syncStoredWidth);
  }, []);

  useEffect(() => {
    if (!isResizing) return;

    const previousCursor = document.body.style.cursor;
    const previousUserSelect = document.body.style.userSelect;
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";

    const handlePointerMove = (event: PointerEvent) => {
      event.preventDefault();
      const dragOffset = event.clientX - resizeStart.current.pointerX;
      const next = clampSidebarWidth(resizeStart.current.width + dragOffset);
      currentWidth.current = next;
      setSidebarWidth(next);
    };
    const finishResize = () => {
      persistSidebarWidth(currentWidth.current);
      setIsResizing(false);
    };

    window.addEventListener("pointermove", handlePointerMove);
    window.addEventListener("pointerup", finishResize);
    window.addEventListener("pointercancel", finishResize);
    return () => {
      document.body.style.cursor = previousCursor;
      document.body.style.userSelect = previousUserSelect;
      window.removeEventListener("pointermove", handlePointerMove);
      window.removeEventListener("pointerup", finishResize);
      window.removeEventListener("pointercancel", finishResize);
    };
  }, [isResizing]);

  const resetSidebarWidth = () => {
    currentWidth.current = DEFAULT_SIDEBAR_WIDTH;
    setSidebarWidth(undefined);
    persistSidebarWidth(undefined);
  };

  const startSidebarResize = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 || !sidebarRef.current) return;
    event.preventDefault();
    event.stopPropagation();
    resizeStart.current = {
      pointerX: event.clientX,
      width: sidebarRef.current.getBoundingClientRect().width,
    };
    currentWidth.current = resizeStart.current.width;
    setIsResizing(true);
  };

  const resizeSidebarWithKeyboard = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home"].includes(event.key)) return;
    event.preventDefault();
    if (event.key === "Home") {
      resetSidebarWidth();
      return;
    }
    const direction = event.key === "ArrowRight" ? 1 : -1;
    const renderedWidth = sidebarRef.current?.getBoundingClientRect().width
      ?? sidebarWidth
      ?? DEFAULT_SIDEBAR_WIDTH;
    const next = clampSidebarWidth(
      renderedWidth + direction * SIDEBAR_KEYBOARD_STEP,
    );
    currentWidth.current = next;
    setSidebarWidth(next);
    persistSidebarWidth(next);
  };

  return {
    sidebarRef,
    sidebarWidth,
    sidebarStyle,
    isResizing,
    resetSidebarWidth,
    startSidebarResize,
    resizeSidebarWithKeyboard,
  };
}
