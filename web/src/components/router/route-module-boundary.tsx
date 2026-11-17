import { Component, type ReactNode } from "react";
import { Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";

class RouteModuleErrorBoundary extends Component<
  {
    children: ReactNode;
    title: string;
    description: string;
    retryLabel: string;
  },
  { failed: boolean }
> {
  state = { failed: false };

  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true };
  }

  override render() {
    if (this.state.failed) {
      return (
        <div className="flex min-h-[40vh] items-center justify-center px-4">
          <div className="max-w-md rounded-lg border border-border bg-card px-6 py-5 text-center shadow-sm">
            <div className="text-sm font-semibold text-foreground">{this.props.title}</div>
            <p className="mt-2 text-sm text-muted-foreground">{this.props.description}</p>
            <button
              type="button"
              className="mt-4 rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground"
              onClick={() => window.location.reload()}
            >
              {this.props.retryLabel}
            </button>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}

export function RouteModuleFallback() {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-[40vh] items-center justify-center px-4 text-muted-foreground">
      <div className="flex items-center gap-2 text-sm">
        <Loader2 className="size-4 animate-spin" />
        {t("common.loading")}
      </div>
    </div>
  );
}

export function RouteModuleBoundary({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <RouteModuleErrorBoundary
      title={t("common.moduleLoadErrorTitle")}
      description={t("common.moduleLoadErrorDesc")}
      retryLabel={t("common.retry")}
    >
      {children}
    </RouteModuleErrorBoundary>
  );
}
