import { Component, lazy, Suspense, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Sidebar } from "./sidebar";
import { Topbar } from "./topbar";
import { AssistantProvider, useAssistant } from "@/components/ai/assistant-provider";

const AssistantPanel = lazy(async () => {
  const module = await import("@/components/ai/assistant-panel");
  return { default: module.AssistantPanel };
});

class OptionalModuleBoundary extends Component<
  { children: ReactNode; fallback: ReactNode },
  { failed: boolean }
> {
  state = { failed: false };

  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true };
  }

  override render() {
    return this.state.failed ? this.props.fallback : this.props.children;
  }
}

function AssistantPanelUnavailable() {
  const { t } = useTranslation();
  const { setOpen } = useAssistant();

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 px-4">
      <div
        role="alert"
        className="max-w-md rounded-lg border border-border bg-card px-6 py-5 text-center shadow-lg"
      >
        <div className="text-sm font-semibold text-foreground">
          {t("common.moduleLoadErrorTitle")}
        </div>
        <p className="mt-2 text-sm text-muted-foreground">{t("common.moduleLoadErrorDesc")}</p>
        <button
          type="button"
          className="mt-4 rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground"
          onClick={() => setOpen(false)}
        >
          {t("common.close")}
        </button>
      </div>
    </div>
  );
}

function LazyAssistantPanel() {
  const { open } = useAssistant();
  if (!open) return null;

  return (
    <OptionalModuleBoundary fallback={<AssistantPanelUnavailable />}>
      <Suspense fallback={null}>
        <AssistantPanel />
      </Suspense>
    </OptionalModuleBoundary>
  );
}

/**
 * App layout skeleton: fixed sidebar + top bar + scrollable content region.
 * This is the ONLY place that composes the raw layout; everything inside the
 * content slot is a shadcn-styled page. The AI assistant state lives here so
 * its history survives navigation, while the panel module loads only when it
 * is opened.
 */
export function AppShell({ children, blockReplay = false }: { children: ReactNode; blockReplay?: boolean }) {
  return (
    <AssistantProvider>
      <div className="flex h-screen w-full overflow-hidden bg-sidebar text-foreground">
        <Sidebar />
        <div className="flex min-w-0 flex-1 flex-col bg-sidebar">
          <Topbar />
          <main
            data-lo-block={blockReplay ? "" : undefined}
            className="min-h-0 flex-1 overflow-auto rounded-tl-xl border-l border-t border-sidebar-border bg-background"
          >
            {children}
          </main>
        </div>
      </div>
      <LazyAssistantPanel />
    </AssistantProvider>
  );
}
