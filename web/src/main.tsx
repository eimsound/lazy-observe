import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { ErrorBoundary } from "@lazy-observe/react";
import i18n from "@/lib/i18n";
import { ThemeProvider } from "@/lib/theme/theme-provider";
import { AuthProvider } from "@/lib/auth/auth-provider";
import { handleSessionExpiry } from "@/lib/auth/session-expiry";
import { registerSessionExpiryHandler } from "@/lib/api/client";
import { SidebarProvider } from "@/hooks/use-sidebar";
import { TooltipProvider } from "@/components/ui/tooltip";
import { router } from "@/router";
import "@/styles/globals.css";

function renderAppCrashFallback(_error: Error, reset: () => void) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4 text-foreground">
      <div className="max-w-md rounded-xl border border-border bg-card px-6 py-5 text-center shadow-sm">
        <div className="text-sm font-semibold text-foreground">{i18n.t("common.appCrashTitle")}</div>
        <p className="mt-2 text-sm text-muted-foreground">{i18n.t("common.appCrashDesc")}</p>
        <button
          type="button"
          className="mt-4 h-8 rounded-md bg-primary px-3 text-xs font-medium text-primary-foreground"
          onClick={() => {
            reset();
            window.location.reload();
          }}
        >
          {i18n.t("common.reload")}
        </button>
      </div>
    </div>
  );
}

// Any request that comes back as a bare 401 means the session expired: sign
// out locally so the authed layout sends the user back to /login.
const queryClient: QueryClient = new QueryClient({
  queryCache: new QueryCache({ onError: (error) => handleSessionExpiry(queryClient, error) }),
  mutationCache: new MutationCache({ onError: (error) => handleSessionExpiry(queryClient, error) }),
  defaultOptions: {
    queries: {
      refetchOnWindowFocus: false,
      staleTime: 30_000,
    },
  },
});

registerSessionExpiryHandler((error) => handleSessionExpiry(queryClient, error));

const rootEl = document.getElementById("root");
if (!rootEl) throw new Error("#root not found");

createRoot(rootEl).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <ErrorBoundary fallback={renderAppCrashFallback}>
          <AuthProvider>
            <SidebarProvider>
              <TooltipProvider delayDuration={200}>
                <RouterProvider router={router} />
              </TooltipProvider>
            </SidebarProvider>
          </AuthProvider>
        </ErrorBoundary>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
);
