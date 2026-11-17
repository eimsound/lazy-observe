import dayjs from "../dayjs.ts";

/** Profile metadata as serialized by the gateway. */
export interface ProfileRecordDTO {
  id: number;
  service: string;
  type: ProfileType;
  start: string;
  end: string;
  size: number;
  trace_id?: string;
}

export interface ProfileServicesResponseDTO {
  services: string[];
}

export interface ProfileListResponseDTO {
  profiles: ProfileRecordDTO[];
}

/** Sampled profile kinds. `goroutine` only appears when the SDK captured it. */
export type ProfileType = "cpu" | "heap" | "goroutine";

export const PROFILE_TYPES: ProfileType[] = ["cpu", "heap", "goroutine"];

/** Browser-safe profile metadata after normalizing the wire contract. */
export interface ProfileRecord {
  id: string;
  service: string;
  type: ProfileType;
  /** Epoch milliseconds. */
  startMs: number;
  /** Epoch milliseconds. */
  endMs: number;
  size: number;
  traceId?: string;
}

const RFC3339 =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

function isProfileType(value: unknown): value is ProfileType {
  return value === "cpu" || value === "heap" || value === "goroutine";
}

function rfc3339Millis(value: unknown, field: "start" | "end"): number {
  if (typeof value !== "string" || !RFC3339.test(value)) {
    throw new TypeError(`profile ${field} must be RFC3339`);
  }
  const millis = dayjs(value).valueOf();
  if (!Number.isFinite(millis)) {
    throw new TypeError(`profile ${field} must be RFC3339`);
  }
  return millis;
}

export function normalizeProfileRecord(dto: ProfileRecordDTO): ProfileRecord {
  if (!Number.isSafeInteger(dto.id) || dto.id <= 0) {
    throw new TypeError("profile id must be a positive integer");
  }
  if (typeof dto.service !== "string" || dto.service.trim() === "") {
    throw new TypeError("profile service must be a non-empty string");
  }
  if (!isProfileType(dto.type)) {
    throw new TypeError("profile type is invalid");
  }
  if (!Number.isSafeInteger(dto.size) || dto.size < 0) {
    throw new TypeError("profile size must be a non-negative integer");
  }

  const startMs = rfc3339Millis(dto.start, "start");
  const endMs = rfc3339Millis(dto.end, "end");
  if (endMs < startMs) {
    throw new TypeError("profile end precedes start");
  }
  const traceId = typeof dto.trace_id === "string" ? dto.trace_id.trim() : "";

  return {
    id: String(dto.id),
    service: dto.service,
    type: dto.type,
    startMs,
    endMs,
    size: dto.size,
    ...(traceId ? { traceId } : {}),
  };
}

export function normalizeProfileServicesResponse(
  response: ProfileServicesResponseDTO,
): string[] {
  if (!response || !Array.isArray(response.services)) {
    throw new TypeError("profile services response must contain services");
  }
  if (!response.services.every((service) => typeof service === "string")) {
    throw new TypeError("profile services must be strings");
  }
  return response.services;
}

export function normalizeProfileListResponse(response: ProfileListResponseDTO): ProfileRecord[] {
  if (!response || !Array.isArray(response.profiles)) {
    throw new TypeError("profile list response must contain profiles");
  }
  return response.profiles.map(normalizeProfileRecord);
}
