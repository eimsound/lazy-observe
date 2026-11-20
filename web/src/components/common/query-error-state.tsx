import { AlertCircle, ShieldAlert } from "lucide-react";
import { useTranslation } from "react-i18next";
import { formatErrorMessage, isForbiddenError } from "@/lib/api/errors";
import { EmptyState } from "@/components/common/empty-state";
import { Button } from "@/components/ui/button";

/**
 * Renders a clear error or 403 forbidden state instead of letting failed queries
 * fall through into a misleading empty-list state.
 */
export function QueryErrorState({
  error,
  onRetry,
  forbiddenTitle,
  forbiddenDesc,
  errorTitle,
}: {
  error: unknown;
  onRetry?: () => void;
  forbiddenTitle?: string;
  forbiddenDesc?: string;
  errorTitle?: string;
}) {
  const { t } = useTranslation();

  if (isForbiddenError(error)) {
    return (
      <EmptyState
        icon={<ShieldAlert className="size-6 text-destructive" />}
        title={forbiddenTitle ?? t("common.forbiddenTitle")}
        desc={forbiddenDesc ?? t("common.forbiddenDesc")}
      />
    );
  }

  return (
    <EmptyState
      icon={<AlertCircle className="size-6 text-destructive" />}
      title={errorTitle ?? t("common.loadFailed")}
      desc={formatErrorMessage(error, t("common.queryErrorDesc"))}
      action={
        onRetry ? (
          <Button variant="outline" size="sm" onClick={onRetry}>
            {t("common.retry")}
          </Button>
        ) : undefined
      }
    />
  );
}
