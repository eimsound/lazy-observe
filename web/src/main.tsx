import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import "@/lib/i18n";
import { ThemeProvider } from "@/lib/theme/theme-provider";
import { AuthProvider } from "@/lib/auth/auth-provider";
import { handleSessionExpiry } from "@/lib/auth/session-expiry";
import { registerSessionExpiryHandler } from "@/lib/api/client";
import { SidebarProvider } from "@/hooks/use-sidebar";
import { TooltipProvider } from "@/components/ui/tooltip";
import { router } from "@/router";
import "@/styles/globals.css";

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
        <AuthProvider>
          <SidebarProvider>
            <TooltipProvider delayDuration={200}>
              <RouterProvider router={router} />
            </TooltipProvider>
          </SidebarProvider>
        </AuthProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
);
