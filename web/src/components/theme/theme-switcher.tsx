import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Check, Monitor, Moon, Palette, RotateCcw, Sun } from "lucide-react";
import { useTheme } from "@/lib/theme/theme-provider";
import { FONT_SIZES, PALETTES, RADII, type ThemeMode } from "@/lib/theme/presets";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
  SheetTrigger,
} from "@/components/ui/sheet";
import { Separator } from "@/components/ui/separator";

/**
 * Full theme panel (requirements §6), rendered in a right-side Sheet. Covers all
 * four axes — mode (tri-state), preset palette (6+), corner radius, and font-size
 * tier — plus a reset button. Every change writes CSS variables via ThemeProvider
 * and persists to localStorage; charts follow automatically through useChartTheme.
 */
export function ThemeSwitcher() {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const {
    mode,
    setMode,
    palette,
    setPalette,
    radius,
    setRadius,
    font,
    setFont,
    reset,
  } = useTheme();

  const modes: Array<{ id: ThemeMode; icon: typeof Sun; labelKey: string }> = [
    { id: "light", icon: Sun, labelKey: "theme.light" },
    { id: "dark", icon: Moon, labelKey: "theme.dark" },
    { id: "system", icon: Monitor, labelKey: "theme.system" },
  ];

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button variant="ghost" size="icon" aria-label={t("theme.title")}>
          <Palette className="size-4" />
        </Button>
      </SheetTrigger>
      <SheetContent side="right" className="w-full sm:max-w-sm">
        <SheetHeader>
          <SheetTitle>{t("theme.title")}</SheetTitle>
          <SheetDescription>{t("theme.panelSubtitle")}</SheetDescription>
        </SheetHeader>

        <div className="flex-1 overflow-y-auto p-4">
          {/* Mode */}
          <Section title={t("theme.mode")}>
            <div className="grid grid-cols-3 gap-2">
              {modes.map((m) => {
                const Icon = m.icon;
                const active = mode === m.id;
                return (
                  <Button
                    key={m.id}
                    variant={active ? "default" : "outline"}
                    size="sm"
                    className="h-auto flex-col gap-1 py-2"
                    onClick={() => setMode(m.id)}
                  >
                    <Icon className="size-4" />
                    <span className="text-[11px]">{t(m.labelKey)}</span>
                  </Button>
                );
              })}
            </div>
          </Section>

          <Separator className="my-4" />

          {/* Palette */}
          <Section title={t("theme.palette")}>
            <div className="grid grid-cols-4 gap-2">
              {PALETTES.map((p) => {
                const active = palette === p.id;
                return (
                  <button
                    key={p.id || "neutral"}
                    onClick={() => setPalette(p.id)}
                    title={t(p.labelKey)}
                    aria-label={t(p.labelKey)}
                    className={cn(
                      "relative flex h-9 items-center justify-center rounded-md border transition-colors",
                      active ? "border-ring ring-2 ring-ring" : "border-border",
                    )}
                    style={{ backgroundColor: p.swatch }}
                  >
                    {active && <Check className="size-4 text-white drop-shadow" />}
                  </button>
                );
              })}
            </div>
          </Section>

          <Separator className="my-4" />

          {/* Radius */}
          <Section title={t("theme.radius")}>
            <div className="grid grid-cols-5 gap-2">
              {RADII.map((r) => {
                const active = radius === r.value;
                return (
                  <Button
                    key={r.id}
                    variant={active ? "default" : "outline"}
                    size="sm"
                    className="px-0"
                    onClick={() => setRadius(r.value)}
                  >
                    {r.label}
                  </Button>
                );
              })}
            </div>
          </Section>

          <Separator className="my-4" />

          {/* Font size */}
          <Section title={t("theme.fontSize")}>
            <div className="grid grid-cols-3 gap-2">
              {FONT_SIZES.map((f) => {
                const active = font === f.value;
                return (
                  <Button
                    key={f.id}
                    variant={active ? "default" : "outline"}
                    size="sm"
                    onClick={() => setFont(f.value)}
                  >
                    {t(f.labelKey)}
                  </Button>
                );
              })}
            </div>
          </Section>
        </div>

        <div className="border-t border-border p-4">
          <Button variant="outline" size="sm" className="w-full gap-2" onClick={reset}>
            <RotateCcw className="size-4" />
            {t("theme.resetDefaults")}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="mb-2 text-xs font-medium text-muted-foreground">{title}</div>
      {children}
    </div>
  );
}
