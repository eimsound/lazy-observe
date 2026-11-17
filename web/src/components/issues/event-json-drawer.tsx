import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Check, Copy } from "lucide-react";
import type { ErrorEvent } from "@/lib/api/issues";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";

/** Right-side drawer showing the raw protocol-v1 error event JSON, with copy. */
export function EventJsonDrawer({
  event,
  open,
  onOpenChange,
}: {
  event: ErrorEvent | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  const json = event ? JSON.stringify(event, null, 2) : "";

  function copy() {
    void navigator.clipboard?.writeText(json).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full sm:max-w-2xl" hideClose>
        <SheetHeader>
          <div className="flex items-center justify-between gap-3 pr-2">
            <SheetTitle>{t("issues.rawJsonTitle")}</SheetTitle>
            <Button size="sm" variant="outline" onClick={copy} disabled={!event}>
              {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
              {copied ? t("common.copied") : t("issues.copyJson")}
            </Button>
          </div>
        </SheetHeader>
        <div className="min-h-0 flex-1 overflow-auto px-4 pb-4">
          <pre className="rounded-md border border-border bg-muted/40 p-3 font-mono text-xs leading-relaxed text-foreground">
            {json}
          </pre>
        </div>
      </SheetContent>
    </Sheet>
  );
}
