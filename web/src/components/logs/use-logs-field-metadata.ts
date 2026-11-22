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

const EMPTY_FIELD_VALUES: Record<string, LogsFieldValue[]> = {};
const EMPTY_FIELD_LOADING: Record<string, boolean> = {};
const EMPTY_FIELD_ERRORS: Record<string, string | null> = {};

interface FieldMetadataCache {
  identity: string;
  values: Record<string, LogsFieldValue[]>;
  loading: Record<string, boolean>;
  errors: Record<string, string | null>;
}

interface FieldValueRequestToken {
  identity: string;
  seq: number;
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
  const fieldValueRequests = useRef<Record<string, FieldValueRequestToken>>({});
  const nextFieldValueRequestSeq = useRef(0);

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

  const [cache, setCache] = useState<FieldMetadataCache>({
    identity: metadataIdentity,
    values: EMPTY_FIELD_VALUES,
    loading: EMPTY_FIELD_LOADING,
    errors: EMPTY_FIELD_ERRORS,
  });

  useEffect(() => {
    for (const [field, token] of Object.entries(fieldValueRequests.current)) {
      if (token.identity !== metadataIdentity) {
        delete fieldValueRequests.current[field];
      }
    }
  }, [metadataIdentity]);

  const activeCache = cache.identity === metadataIdentity ? cache : null;
  const fieldValues = activeCache?.values ?? EMPTY_FIELD_VALUES;
  const fieldValuesLoading = activeCache?.loading ?? EMPTY_FIELD_LOADING;
  const fieldValuesErrors = activeCache?.errors ?? EMPTY_FIELD_ERRORS;

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
    const requestToken: FieldValueRequestToken = {
      identity: metadataIdentity,
      seq: ++nextFieldValueRequestSeq.current,
    };
    fieldValueRequests.current[field] = requestToken;
    const isCurrentRequest = () => fieldValueRequests.current[field] === requestToken;
    const updateCache = (mutate: (base: FieldMetadataCache) => FieldMetadataCache) => {
      setCache((current) => {
        const base = current.identity === metadataIdentity
          ? current
          : {
            identity: metadataIdentity,
            values: EMPTY_FIELD_VALUES,
            loading: EMPTY_FIELD_LOADING,
            errors: EMPTY_FIELD_ERRORS,
          };
        return mutate(base);
      });
    };
    updateCache((base) => ({
      ...base,
      loading: { ...base.loading, [field]: true },
      errors: { ...base.errors, [field]: null },
    }));
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
      if (!isCurrentRequest()) return;
      updateCache((base) => ({
        ...base,
        values: { ...base.values, [field]: response.values },
      }));
    }).catch((error: unknown) => {
      if (!isCurrentRequest()) return;
      updateCache((base) => ({
        ...base,
        errors: { ...base.errors, [field]: errorMessage(error) },
      }));
    }).finally(() => {
      if (!isCurrentRequest()) return;
      updateCache((base) => ({
        ...base,
        loading: { ...base.loading, [field]: false },
      }));
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
