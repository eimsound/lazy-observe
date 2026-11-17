import { useTranslation } from "react-i18next";
import { Plus, X } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import type { Pair } from "./kv-pairs";

/**
 * A small key/value list editor used for alert-rule labels and annotations.
 * Rows can be added/removed; values flow back through `onChange`. Purely
 * token-driven, no hardcoded colors.
 */
export function KvEditor({
  pairs,
  onChange,
}: {
  pairs: Pair[];
  onChange: (next: Pair[]) => void;
}) {
  const { t } = useTranslation();

  const update = (i: number, patch: Partial<Pair>) =>
    onChange(pairs.map((p, idx) => (idx === i ? { ...p, ...patch } : p)));
  const remove = (i: number) => onChange(pairs.filter((_, idx) => idx !== i));
  const add = () => onChange([...pairs, { key: "", value: "" }]);

  return (
    <div className="space-y-2">
      {pairs.map((p, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            value={p.key}
            onChange={(e) => update(i, { key: e.target.value })}
            placeholder={t("alerts.keyPlaceholder")}
            className="font-mono text-sm"
            spellCheck={false}
          />
          <Input
            value={p.value}
            onChange={(e) => update(i, { value: e.target.value })}
            placeholder={t("alerts.valuePlaceholder")}
            className="font-mono text-sm"
            spellCheck={false}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            onClick={() => remove(i)}
            aria-label={t("common.remove")}
          >
            <X className="size-4" />
          </Button>
        </div>
      ))}
      <Button type="button" variant="outline" size="sm" onClick={add} className="gap-1.5">
        <Plus className="size-3.5" />
        {t("alerts.addPair")}
      </Button>
    </div>
  );
}
