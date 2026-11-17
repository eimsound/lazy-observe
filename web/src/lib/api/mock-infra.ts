import type { Host } from "./hosts";
import type {
  ScrapeTarget,
  ScrapeTargetInput,
  TargetHealth,
} from "./scrape-targets";
import type { AlertConfig } from "./alert-config";
import { hexId } from "./mock-random";
import dayjs from "../dayjs.ts";

/**
 * In-memory infrastructure store for VITE_MOCK mode: hosts, scrape targets, and
 * per-project alert config. Scrape targets are mutable for the tab lifetime so
 * the full CRUD surface is interactive offline; host rows are static, spanning
 * the good/needs-improvement/poor usage buckets so threshold coloring is
 * visible at a glance (one host is intentionally down).
 */

let counter = 1;
const nextId = (prefix: string) => `${prefix}_${hexId(`${prefix}${counter++}`, 10)}`;
const ago = (secs: number) => dayjs().subtract(secs, "second").toISOString();

// ── hosts ──────────────────────────────────────────────────────────────────

export const mockHosts: Host[] = [
  {
    instance: "10.0.0.11:9100",
    hostname: "web-prod-01",
    up: true,
    cpu_cores: 8,
    load1: 2.1,
    mem_used_percent: 46.3,
    disk_used_percent_max: 58.0,
  },
  {
    instance: "10.0.0.12:9100",
    hostname: "web-prod-02",
    up: true,
    cpu_cores: 8,
    load1: 6.4,
    mem_used_percent: 78.9,
    disk_used_percent_max: 71.2,
  },
  {
    instance: "10.0.0.13:9100",
    hostname: "db-prod-01",
    up: true,
    cpu_cores: 16,
    load1: 15.8,
    mem_used_percent: 92.4,
    disk_used_percent_max: 88.7,
  },
  {
    instance: "10.0.0.14:9100",
    hostname: "cache-prod-01",
    up: true,
    cpu_cores: 4,
    load1: 0.6,
    mem_used_percent: 33.1,
    disk_used_percent_max: 24.5,
  },
  {
    instance: "10.0.0.21:9100",
    hostname: "worker-prod-03",
    up: false,
    cpu_cores: null,
    load1: null,
    mem_used_percent: null,
    disk_used_percent_max: 95.3,
  },
];

/** Instance list reused by the host detail metric charts. */
export const MOCK_HOST_INSTANCES = mockHosts.map((h) => h.instance);

// ── scrape targets ───────────────────────────────────────────────────────────

export const mockScrapeTargets: ScrapeTarget[] = [
  {
    id: "tgt_node_web01",
    name: "node · web-prod-01",
    url: "http://10.0.0.11:9100/metrics",
    interval: "15s",
    labels: { job: "node", env: "prod" },
    enabled: true,
  },
  {
    id: "tgt_node_web02",
    name: "node · web-prod-02",
    url: "http://10.0.0.12:9100/metrics",
    interval: "15s",
    labels: { job: "node", env: "prod" },
    enabled: true,
  },
  {
    id: "tgt_mysql_primary",
    name: "mysqld-exporter · primary",
    url: "http://10.0.0.13:9104/metrics",
    interval: "30s",
    labels: { job: "mysql", env: "prod" },
    enabled: true,
  },
  {
    id: "tgt_blackbox_api",
    name: "blackbox · api health",
    url: "http://10.0.0.30:9115/probe?target=https%3A%2F%2Fapi.example.com",
    interval: "1m",
    labels: { job: "blackbox" },
    enabled: false,
  },
];

export function createMockTarget(input: ScrapeTargetInput): ScrapeTarget {
  const target: ScrapeTarget = { id: nextId("tgt"), ...input };
  mockScrapeTargets.unshift(target);
  return target;
}

export function updateMockTarget(id: string, input: ScrapeTargetInput): ScrapeTarget {
  const idx = mockScrapeTargets.findIndex((t) => t.id === id);
  if (idx < 0) throw new Error("scrape target not found");
  mockScrapeTargets[idx] = { id, ...input };
  return mockScrapeTargets[idx];
}

export function deleteMockTarget(id: string): void {
  const idx = mockScrapeTargets.findIndex((t) => t.id === id);
  if (idx >= 0) mockScrapeTargets.splice(idx, 1);
}

/**
 * Derive a stable health for a target: disabled -> unknown (never scraped);
 * otherwise up, except a deterministic subset (by id hash) that reports down so
 * the down state is exercised. last_scrape_at is a few seconds ago for scraped
 * targets.
 */
export function mockTargetHealth(id: string): TargetHealth {
  const target = mockScrapeTargets.find((t) => t.id === id);
  if (!target || !target.enabled) {
    return { status: "unknown", last_scrape_at: null, scrape_job: `scrape-${id}` };
  }
  const down = parseInt(hexId(id, 4), 16) % 5 === 0;
  return {
    status: down ? "down" : "up",
    last_scrape_at: ago(down ? 120 : 8),
    scrape_job: `scrape-${id}`,
  };
}

// ── project alert config ─────────────────────────────────────────────────────

const alertConfigs: Record<string, AlertConfig> = {};

const DEFAULT_ALERT_CONFIG: AlertConfig = {
  new_issue: true,
  regression: true,
  error_rate_threshold_per_min: 10,
  channel_ids: ["chn_ops_webhook"],
};

export function getMockAlertConfig(projectId: string): AlertConfig {
  return { ...(alertConfigs[projectId] ?? DEFAULT_ALERT_CONFIG) };
}

export function setMockAlertConfig(projectId: string, config: AlertConfig): AlertConfig {
  alertConfigs[projectId] = { ...config };
  return { ...config };
}
