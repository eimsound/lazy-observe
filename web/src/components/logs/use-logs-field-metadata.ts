import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import {
  queryLogFieldNames,
  queryLogFieldValues,
  queryLogStreamFieldNames,
  queryLogStreamFieldValues,
  type LogsFieldValue,
} from "@/lib/api/logs";
import {
  DEFAULT_LOGS_QUERY,
  buildLogsExtraFilterParams,
  logsAutocompleteBaseQuery,
  type LogsAutocompleteContext,
  type LogsExtraFilter,
  type LogsExtraFilterParams,
} from "@/lib/logs-explorer";
import { useProjectQuery } from "@/lib/project/use-project-query";
import type { LogsQLEditorSuggestion } from "./logsql-editor";

interface UseLogsFieldMetadataOptions {
  project: string;
  executedQuery: string | undefined;
  executedFilters: LogsExtraFilter[] | undefined;
  filters: LogsExtraFilter[];
  filtersForRequest: LogsExtraFilterParams;
  metadataRange: { start: number; end: number };
  enabled: boolean;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function useLogsFieldMetadata({
  project,
  executedQuery,
  executedFilters,
  filters,
  filtersForRequest,
  metadataRange,
  enabled,
}: UseLogsFieldMetadataOptions) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [fieldValues, setFieldValues] = useState<Record<string, LogsFieldValue[]>>({});
  const [fieldValuesLoading, setFieldValuesLoading] = useState<Record<string, boolean>>({});
  const [fieldValuesErrors, setFieldValuesErrors] = useState<Record<string, string | null>>({});
  const fieldValueRequestKeys = useRef<Record<string, string>>({});

  const metadataQuery = executedQuery || DEFAULT_LOGS_QUERY;
  const metadataFilters = useMemo(
    () => buildLogsExtraFilterParams(executedFilters ?? filters),
    [executedFilters, filters],
  );
  const metadataIdentity = useMemo(() => JSON.stringify([
    project,
    metadataQuery,
    metadataRange.start,
    metadataRange.end,
    metadataFilters.extraFilters,
    metadataFilters.extraStreamFilters,
  ]), [
    metadataFilters.extraFilters,
    metadataFilters.extraStreamFilters,
    metadataQuery,
    metadataRange.end,
    metadataRange.start,
    project,
  ]);

  useEffect(() => {
    fieldValueRequestKeys.current = {};
    setFieldValues({});
    setFieldValuesLoading({});
    setFieldValuesErrors({});
  }, [metadataIdentity]);

  const fieldNamesResult = useProjectQuery({
    queryKey: [
      "logs-explorer",
      "field-names",
      metadataQuery,
      metadataRange,
      metadataFilters,
    ],
    enabled,
    staleTime: 30_000,
    retry: false,
    queryFn: ({ signal, project: activeProject }) => queryLogFieldNames({
      query: metadataQuery,
      start: metadataRange.start,
      end: metadataRange.end,
      limit: 1_000,
      project: activeProject,
      ...metadataFilters,
    }, signal),
  });

  const streamFieldsResult = useProjectQuery({
    queryKey: [
      "logs-explorer",
      "stream-fields",
      metadataQuery,
      metadataRange,
      metadataFilters,
    ],
    enabled,
    staleTime: 30_000,
    retry: false,
    queryFn: ({ signal, project: activeProject }) => queryLogStreamFieldNames({
      query: metadataQuery,
      start: metadataRange.start,
      end: metadataRange.end,
      limit: 1_000,
      project: activeProject,
      ...metadataFilters,
    }, signal),
  });

  const allFields = useMemo(() => {
    const fields = new Set<string>(["_time", "_msg", "_stream"]);
    fieldNamesResult.data?.values.forEach((field) => fields.add(field.value));
    streamFieldsResult.data?.values.forEach((field) => fields.add(field.value));
    return [...fields].sort((left, right) => left.localeCompare(right));
  }, [fieldNamesResult.data, streamFieldsResult.data]);

  const requestFieldValues = useCallback((field: string) => {
    const activeFilters = executedFilters ?? filters;
    const fieldFilters = buildLogsExtraFilterParams(
      activeFilters.filter((filter) => filter.field !== field),
    );
    const requestKey = JSON.stringify([metadataIdentity, field]);
    fieldValueRequestKeys.current[field] = requestKey;
    setFieldValuesLoading((current) => ({ ...current, [field]: true }));
    setFieldValuesErrors((current) => ({ ...current, [field]: null }));
    void queryClient.fetchQuery({
      queryKey: [
        "logs-explorer",
        project,
        "stream-field-values",
        metadataQuery,
        metadataRange,
        activeFilters,
        field,
      ],
      staleTime: 30_000,
      queryFn: ({ signal }) => queryLogStreamFieldValues({
        field,
        query: metadataQuery,
        start: metadataRange.start,
        end: metadataRange.end,
        limit: 1_000,
        project,
        ...fieldFilters,
      }, signal),
    }).then((response) => {
      if (fieldValueRequestKeys.current[field] !== requestKey) return;
      setFieldValues((current) => ({ ...current, [field]: response.values }));
    }).catch((error: unknown) => {
      if (fieldValueRequestKeys.current[field] !== requestKey) return;
      setFieldValuesErrors((current) => ({
        ...current,
        [field]: errorMessage(error),
      }));
    }).finally(() => {
      if (fieldValueRequestKeys.current[field] !== requestKey) return;
      setFieldValuesLoading((current) => ({ ...current, [field]: false }));
    });
  }, [
    executedFilters,
    filters,
    metadataIdentity,
    metadataQuery,
    metadataRange,
    project,
    queryClient,
  ]);

  const completeQuery = useCallback(async (
    context: LogsAutocompleteContext,
    signal: AbortSignal,
    currentQuery: string,
  ): Promise<LogsQLEditorSuggestion[]> => {
    if (context.kind === "pipe") return [];
    if (context.kind === "field") {
      return allFields.map((value) => ({
        value,
        kind: "field" as const,
        detail: t("logs.fieldSuggestion"),
      }));
    }
    const autocompleteQuery = logsAutocompleteBaseQuery(currentQuery, context);
    const values = await queryClient.fetchQuery({
      queryKey: [
        "logs-explorer",
        project,
        "autocomplete-values",
        metadataIdentity,
        autocompleteQuery,
        context.field,
        context.token,
      ],
      staleTime: 30_000,
      queryFn: ({ signal: querySignal }) => queryLogFieldValues({
        field: context.field,
        query: autocompleteQuery,
        start: metadataRange.start,
        end: metadataRange.end,
        filter: context.token,
        limit: 100,
        project,
        ...filtersForRequest,
      }, AbortSignal.any([signal, querySignal])),
    });
    return values.values.map((item) => {
      const escaped = item.value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
      return {
        value: context.quoted ? escaped : `"${escaped}"`,
        label: item.value,
        kind: "value" as const,
        detail: t("logs.valueHits", { count: item.hits }),
      };
    });
  }, [
    allFields,
    filtersForRequest,
    metadataIdentity,
    metadataRange,
    project,
    queryClient,
    t,
  ]);

  return {
    allFields,
    streamFields: streamFieldsResult.data?.values ?? [],
    streamFieldsLoading: streamFieldsResult.isFetching,
    streamFieldsError: streamFieldsResult.error ? errorMessage(streamFieldsResult.error) : null,
    fieldValues,
    fieldValuesLoading,
    fieldValuesErrors,
    requestFieldValues,
    completeQuery,
  };
}
