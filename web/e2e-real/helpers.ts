import { readFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { expect, type Page } from "@playwright/test";

export function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} must be set (run via scripts/e2e-web-real.sh or mise run web-e2e-real)`);
  }
  return value;
}

export function readInitialAdminPassword(): string {
  const filePath = requiredEnv("LO_REAL_E2E_ADMIN_PASSWORD_FILE");
  const password = readFileSync(filePath, "utf8").trim();
  if (!password) {
    throw new Error(`initial admin password file is empty: ${filePath}`);
  }
  return password;
}

export function randomHex(bytes: number): string {
  return randomBytes(bytes).toString("hex");
}

export function nowUnixNanos(): bigint {
  return BigInt(Date.now()) * 1_000_000n;
}

// --- Minimal zero-dependency protobuf encoder for OTLP Metrics & Logs ---

function encodeVarint(value: number | bigint): Uint8Array {
  let n = typeof value === "bigint" ? value : BigInt(value);
  const out: number[] = [];
  while (n >= 0x80n) {
    out.push(Number((n & 0x7fn) | 0x80n));
    n >>= 7n;
  }
  out.push(Number(n));
  return new Uint8Array(out);
}

function concatBytes(chunks: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const chunk of chunks) total += chunk.length;
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

function tag(fieldNumber: number, wireType: number): Uint8Array {
  return encodeVarint((fieldNumber << 3) | wireType);
}

function fieldVarint(fieldNumber: number, value: number | bigint): Uint8Array {
  return concatBytes([tag(fieldNumber, 0), encodeVarint(value)]);
}

function fieldFixed64(fieldNumber: number, value: bigint): Uint8Array {
  const buf = new ArrayBuffer(8);
  new DataView(buf).setBigUint64(0, value, true);
  return concatBytes([tag(fieldNumber, 1), new Uint8Array(buf)]);
}

function fieldDouble(fieldNumber: number, value: number): Uint8Array {
  const buf = new ArrayBuffer(8);
  new DataView(buf).setFloat64(0, value, true);
  return concatBytes([tag(fieldNumber, 1), new Uint8Array(buf)]);
}

function fieldBytes(fieldNumber: number, bytes: Uint8Array): Uint8Array {
  return concatBytes([tag(fieldNumber, 2), encodeVarint(bytes.length), bytes]);
}

function fieldString(fieldNumber: number, text: string): Uint8Array {
  return fieldBytes(fieldNumber, new TextEncoder().encode(text));
}

function hexToBytes(hex: string): Uint8Array {
  return Uint8Array.from(Buffer.from(hex, "hex"));
}

function encodeAnyStringValue(value: string): Uint8Array {
  // AnyValue.string_value = 1
  return fieldString(1, value);
}

function encodeKeyValue(key: string, value: string): Uint8Array {
  // KeyValue.key = 1, KeyValue.value = 2
  return concatBytes([fieldString(1, key), fieldBytes(2, encodeAnyStringValue(value))]);
}

function encodeResource(attributes: Record<string, string>): Uint8Array {
  // Resource.attributes = 1
  return concatBytes(
    Object.entries(attributes).map(([key, value]) => fieldBytes(1, encodeKeyValue(key, value))),
  );
}

export function encodeOtlpGaugeMetric(options: {
  name: string;
  value: number;
  resourceAttributes?: Record<string, string>;
  dataPointAttributes?: Record<string, string>;
  timeUnixNano?: bigint;
}): Uint8Array {
  const timestamp = options.timeUnixNano ?? nowUnixNanos();
  const dataPointChunks: Uint8Array[] = [
    // NumberDataPoint.time_unix_nano = 3 (fixed64)
    fieldFixed64(3, timestamp),
    // NumberDataPoint.as_double = 4 (double)
    fieldDouble(4, options.value),
  ];
  for (const [key, value] of Object.entries(options.dataPointAttributes ?? {})) {
    // NumberDataPoint.attributes = 7
    dataPointChunks.push(fieldBytes(7, encodeKeyValue(key, value)));
  }
  const dataPoint = concatBytes(dataPointChunks);
  // Gauge.data_points = 1
  const gauge = fieldBytes(1, dataPoint);
  // Metric.name = 1, Metric.gauge = 5
  const metric = concatBytes([fieldString(1, options.name), fieldBytes(5, gauge)]);
  // ScopeMetrics.metrics = 2
  const scopeMetrics = fieldBytes(2, metric);
  // ResourceMetrics.resource = 1, ResourceMetrics.scope_metrics = 2
  const resourceMetrics = concatBytes([
    fieldBytes(1, encodeResource(options.resourceAttributes ?? {})),
    fieldBytes(2, scopeMetrics),
  ]);
  // ExportMetricsServiceRequest.resource_metrics = 1
  return fieldBytes(1, resourceMetrics);
}

export function encodeOtlpLogRecord(options: {
  body: string;
  serviceName: string;
  severityNumber?: number;
  severityText?: string;
  attributes?: Record<string, string>;
  traceIdHex?: string;
  spanIdHex?: string;
  timeUnixNano?: bigint;
}): Uint8Array {
  const timestamp = options.timeUnixNano ?? nowUnixNanos();
  const recordChunks: Uint8Array[] = [
    // LogRecord.time_unix_nano = 1 (fixed64)
    fieldFixed64(1, timestamp),
    // LogRecord.severity_number = 2 (varint, 9 = INFO)
    fieldVarint(2, options.severityNumber ?? 9),
    // LogRecord.severity_text = 3 (string)
    fieldString(3, options.severityText ?? "INFO"),
    // LogRecord.body = 5 (AnyValue)
    fieldBytes(5, encodeAnyStringValue(options.body)),
  ];
  for (const [key, value] of Object.entries(options.attributes ?? {})) {
    // LogRecord.attributes = 6
    recordChunks.push(fieldBytes(6, encodeKeyValue(key, value)));
  }
  if (options.traceIdHex) {
    // LogRecord.trace_id = 9 (bytes)
    recordChunks.push(fieldBytes(9, hexToBytes(options.traceIdHex)));
  }
  if (options.spanIdHex) {
    // LogRecord.span_id = 10 (bytes)
    recordChunks.push(fieldBytes(10, hexToBytes(options.spanIdHex)));
  }
  const logRecord = concatBytes(recordChunks);
  // ScopeLogs.log_records = 2
  const scopeLogs = fieldBytes(2, logRecord);
  // ResourceLogs.resource = 1, ResourceLogs.scope_logs = 2
  const resourceLogs = concatBytes([
    fieldBytes(1, encodeResource({ "service.name": options.serviceName })),
    fieldBytes(2, scopeLogs),
  ]);
  // ExportLogsServiceRequest.resource_logs = 1
  return fieldBytes(1, resourceLogs);
}

export function buildOtlpTracePayload(options: {
  serviceName: string;
  traceId: string;
  spanId: string;
  operationName: string;
  attributes?: Record<string, string>;
}) {
  const endNs = nowUnixNanos();
  const startNs = endNs - 45_000_000n; // 45ms duration
  return {
    resourceSpans: [
      {
        resource: {
          attributes: [
            { key: "service.name", value: { stringValue: options.serviceName } },
          ],
        },
        scopeSpans: [
          {
            scope: { name: "web-real-e2e" },
            spans: [
              {
                traceId: options.traceId,
                spanId: options.spanId,
                name: options.operationName,
                kind: 2, // SPAN_KIND_SERVER
                startTimeUnixNano: startNs.toString(),
                endTimeUnixNano: endNs.toString(),
                attributes: Object.entries(options.attributes ?? {}).map(([key, value]) => ({
                  key,
                  value: { stringValue: value },
                })),
                status: { code: 1 },
              },
            ],
          },
        ],
      },
    ],
  };
}

export function buildErrorEventPayload(options: {
  eventId: string;
  type: string;
  value: string;
  message: string;
  traceId?: string;
  spanId?: string;
}) {
  return {
    events: [
      {
        event_id: options.eventId,
        timestamp: new Date().toISOString(),
        platform: "javascript",
        level: "error",
        message: options.message,
        exception: {
          type: options.type,
          value: options.value,
          stacktrace: {
            frames: [
              {
                function: "submitRealE2EOrder",
                filename: "src/checkout/submit.ts",
                lineno: 42,
                colno: 13,
                in_app: true,
              },
            ],
          },
        },
        ...(options.traceId
          ? {
            contexts: {
              trace: {
                trace_id: options.traceId,
                ...(options.spanId ? { span_id: options.spanId } : {}),
              },
            },
          }
          : {}),
        session_id: `real-e2e-session-${options.eventId}`,
      },
    ],
  };
}

// --- UI helpers ---

export async function ensureChineseLocale(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.localStorage.setItem("lo-language", "zh");
  });
}

export async function loginViaUI(page: Page, username: string, password: string): Promise<void> {
  await expect(page.locator("#username")).toBeVisible();
  await page.locator("#username").fill(username);
  await page.locator("#password").fill(password);
  await page.locator('button[type="submit"]').click();
}

export async function switchProjectViaSidebar(page: Page, projectSlug: string): Promise<void> {
  await page.getByRole("button", { name: "切换项目" }).click();
  await page.getByRole("menuitem").filter({ hasText: projectSlug }).first().click();
}

export async function signOutViaTopbar(page: Page): Promise<void> {
  await page.getByRole("button", { name: "用户菜单" }).click();
  await page.getByRole("menuitem", { name: "退出登录" }).click();
  await expect(page).toHaveURL(/\/login/);
}
