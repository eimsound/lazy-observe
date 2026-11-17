import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";
import type { AiConfidence } from "@/lib/api/ai";

/**
 * Semantic confidence badge for AI probable-causes. Reuses the palette-
 * independent --vital-* tokens (good/ni/poor) so high/medium/low read as a
 * green/amber/red scale in both light and dark, with no hardcoded colors.
 */

const CONFIDENCE_CHIP: Record<AiConfidence, string> = {
  high: "border-vital-good/30 bg-vital-good/10 text-vital-good",
  medium: "border-vital-ni/30 bg-vital-ni/10 text-vital-ni",
  low: "border-vital-poor/30 bg-vital-poor/10 text-vital-poor",
};

const CONFIDENCE_DOT: Record<AiConfidence, string> = {
  high: "bg-vital-good",
  medium: "bg-vital-ni",
  low: "bg-vital-poor",
};

const CONFIDENCE_LABEL: Record<AiConfidence, string> = {
  high: "aiAnalysis.confidenceHigh",
  medium: "aiAnalysis.confidenceMedium",
  low: "aiAnalysis.confidenceLow",
};

export function ConfidenceBadge({ confidence }: { confidence: AiConfidence }) {
  const { t } = useTranslation();
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium",
        CONFIDENCE_CHIP[confidence],
      )}
    >
      <span className={cn("inline-block size-1.5 rounded-full", CONFIDENCE_DOT[confidence])} aria-hidden />
      {t(CONFIDENCE_LABEL[confidence])}
    </span>
  );
}
