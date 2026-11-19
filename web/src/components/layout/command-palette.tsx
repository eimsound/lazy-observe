import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "@tanstack/react-router";
import { FolderKanban, LayoutGrid, Search, Waypoints } from "lucide-react";
import { NAV_GROUPS } from "@/config/nav";
import { useProject } from "@/lib/project/project-provider";
import { cn } from "@/lib/utils";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";

interface CommandItem {
  id: string;
  icon?: React.ReactNode;
  label: string;
  group: string;
  keywords: string;
  action: () => void;
}

/**
 * Global command palette: page navigation, project switching, issue search,
 * and trace-ID jump. Opened with ⌘K / Ctrl+K; keyboard navigable.
 */
export function CommandPalette({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { projects, setCurrent } = useProject();
  const [query, setQuery] = useState("");
  const [index, setIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) {
      setQuery("");
      setIndex(0);
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [open]);

  const staticCommands = useMemo<CommandItem[]>(() => {
    const items: CommandItem[] = [];
    for (const group of NAV_GROUPS) {
      for (const item of group.items) {
        if (item.adminOnly) continue;
        items.push({
          id: `page:${item.to}`,
          icon: <LayoutGrid className="size-4" />,
          label: t(item.labelKey),
          group: t(group.labelKey),
          keywords: `${t(item.labelKey)} ${item.to}`,
          action: () => {
            onOpenChange(false);
            void navigate({ to: item.to });
          },
        });
      }
    }
    for (const project of projects) {
      items.push({
        id: `project:${project.slug}`,
        icon: <FolderKanban className="size-4" />,
        label: `${project.name} (${project.slug})`,
        group: t("topbar.switchProject"),
        keywords: `${project.name} ${project.slug}`,
        action: () => {
          setCurrent(project.slug);
          onOpenChange(false);
        },
      });
    }
    return items;
  }, [t, navigate, onOpenChange, projects, setCurrent]);

  const commands = useMemo(() => {
    const trimmed = query.trim().toLowerCase();
    const base = trimmed
      ? staticCommands.filter((item) => item.keywords.toLowerCase().includes(trimmed))
      : staticCommands.slice(0, 8);
    if (/^[0-9a-f]{8,32}$/i.test(query.trim())) {
      base.unshift({
        id: "jump:trace",
        icon: <Waypoints className="size-4" />,
        label: t("palette.jumpTrace", { id: query.trim() }),
        group: t("nav.traces"),
        keywords: "",
        action: () => {
          onOpenChange(false);
          void navigate({ to: "/traces/$traceID", params: { traceID: query.trim() } });
        },
      });
    }
    return base.slice(0, 12);
  }, [query, staticCommands, t, navigate, onOpenChange]);

  useEffect(() => setIndex(0), [query]);

  function select(command: CommandItem | undefined) {
    if (!command) return;
    command.action();
  }

  function onKeyDown(event: React.KeyboardEvent) {
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        setIndex((i) => Math.min(i + 1, commands.length - 1));
        break;
      case "ArrowUp":
        event.preventDefault();
        setIndex((i) => Math.max(i - 1, 0));
        break;
      case "Enter":
        event.preventDefault();
        select(commands[index]);
        break;
      case "Escape":
        onOpenChange(false);
        break;
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="top-[15%] max-w-xl translate-y-0 gap-0 p-0" onKeyDown={onKeyDown}>
        <DialogHeader className="sr-only">
          <DialogTitle>{t("topbar.searchPlaceholder")}</DialogTitle>
        </DialogHeader>
        <div className="flex items-center gap-2 border-b border-border px-3">
          <Search className="size-4 shrink-0 text-muted-foreground" />
          <Input
            ref={inputRef}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder={t("palette.placeholder")}
            className="h-11 border-0 bg-transparent px-0 shadow-none focus-visible:ring-0"
          />
        </div>
        <div className="max-h-80 overflow-y-auto p-1.5">
          {commands.length === 0 ? (
            <p className="px-3 py-8 text-center text-sm text-muted-foreground">
              {t("palette.empty")}
            </p>
          ) : (
            commands.map((command, i) => (
              <button
                key={command.id}
                type="button"
                className={cn(
                  "flex w-full items-center gap-2.5 rounded-md px-3 py-2 text-left text-sm",
                  i === index && "bg-accent text-accent-foreground",
                )}
                onMouseEnter={() => setIndex(i)}
                onClick={() => select(command)}
              >
                <span className="text-muted-foreground">{command.icon}</span>
                <span className="min-w-0 flex-1 truncate">{command.label}</span>
                <span className="shrink-0 text-xs text-muted-foreground">{command.group}</span>
              </button>
            ))
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
