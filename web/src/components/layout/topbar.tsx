import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "@tanstack/react-router";
import { LogOut, Search, Sparkles, User } from "lucide-react";
import { Breadcrumbs } from "./breadcrumbs";
import { CommandPalette } from "./command-palette";
import { ThemeSwitcher } from "@/components/theme/theme-switcher";
import { LanguageSwitcher } from "@/components/i18n/language-switcher";
import { useAssistant } from "@/components/ai/assistant-provider";
import { useAuth } from "@/lib/auth/auth-provider";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

/**
 * GitLab-style top bar: breadcrumbs (left), a command-palette placeholder (⌘K),
 * language + theme switchers, and the user menu (username, role, sign out).
 * Project switching lives in the sidebar brand block so the active project is
 * always visible beside the product identity.
 */
export function Topbar() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { user, role, logout } = useAuth();
  const { setOpen: setAssistantOpen } = useAssistant();
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [signOutFailed, setSignOutFailed] = useState(false);

  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setPaletteOpen((open) => !open);
      }
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  async function onSignOut() {
    setSignOutFailed(false);
    try {
      await logout();
    } catch {
      // The server kept the session; stay signed in so the user can retry.
      setSignOutFailed(true);
      return;
    }
    navigate({ to: "/login" });
  }

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 bg-sidebar px-4 text-sidebar-foreground">
      <Breadcrumbs />

      <div className="ml-auto flex items-center gap-2">
        {/* Global search / command palette */}
        <button
          type="button"
          className="hidden h-9 w-56 items-center gap-2 rounded-md border border-input bg-muted/40 px-3 text-sm text-muted-foreground transition-colors hover:bg-muted md:flex"
          aria-label={t("topbar.searchPlaceholder")}
          onClick={() => setPaletteOpen(true)}
        >
          <Search className="size-4" />
          <span className="flex-1 text-left">{t("common.search")}</span>
          <kbd className="pointer-events-none rounded border border-border bg-background px-1.5 font-mono text-[10px]">
            ⌘K
          </kbd>
        </button>

        {/* AI assistant trigger (opens the global conversational panel) */}
        <Button
          variant="outline"
          size="sm"
          className="gap-1.5"
          onClick={() => setAssistantOpen(true)}
        >
          <Sparkles className="size-3.5 text-primary" />
          <span className="hidden sm:inline">{t("topbar.assistant")}</span>
        </Button>

        <Separator orientation="vertical" className="h-6" />

        <LanguageSwitcher />
        <ThemeSwitcher />

        {signOutFailed && (
          <span role="alert" className="text-xs text-destructive">
            {t("topbar.signOutFailed")}
          </span>
        )}

        {/* User menu */}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" aria-label={t("topbar.userMenu")}>
              <User className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-48">
            <DropdownMenuLabel className="flex flex-col gap-0.5">
              <span className="text-xs font-normal text-muted-foreground">
                {t("topbar.signedInAs")}
              </span>
              <span className="truncate">{user?.username}</span>
              {role && (
                <span className="mt-1 inline-flex w-fit items-center rounded border border-border bg-muted/50 px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">
                  {t(`roles.${role}`)}
                </span>
              )}
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={onSignOut}>
              <LogOut className="size-4" />
              {t("topbar.signOut")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
    </header>
  );
}
