import type { IssuedProjectAPIToken, Project, ProjectAPIToken } from "./projects";
import type { ManagedUser } from "./users";
import type { Role } from "./auth";
import { hexId } from "./mock-random";
import dayjs from "../dayjs.ts";

/**
 * In-memory admin store for VITE_MOCK mode (projects, tokens, users). Mutations
 * persist for the lifetime of the tab so create/revoke/delete flows are fully
 * interactive during a demo. Seeded deterministically on first import.
 */

let counter = 1;
const nextId = (prefix: string) => `${prefix}_${hexId(`${prefix}${counter++}`, 10)}`;
const nowIso = () => dayjs().toISOString();
const ago = (mins: number) => dayjs().subtract(mins, "minute").toISOString();

export const mockProjects: Project[] = [
  { id: "prj_default", name: "生产环境", slug: "production", created_at: ago(60 * 24 * 30) },
  { id: "prj_staging", name: "预发布", slug: "staging", created_at: ago(60 * 24 * 12) },
  { id: "prj_demo", name: "演示项目", slug: "demo", created_at: ago(60 * 24 * 3) },
];

export const mockTokens: Record<string, ProjectAPIToken[]> = {
  prj_default: [
    { id: "tok_a1", name: "ingest-primary", prefix: "lo_prod_9f2a", created_at: ago(60 * 24 * 20), last_used_at: ago(4), revoked: false },
    { id: "tok_a2", name: "ingest-otelcol", prefix: "lo_prod_3b7c", created_at: ago(60 * 24 * 8), last_used_at: ago(120), revoked: false },
  ],
  prj_staging: [
    { id: "tok_b1", name: "ci-pipeline", prefix: "lo_stg_5d1e", created_at: ago(60 * 24 * 6), last_used_at: null, revoked: false },
  ],
  prj_demo: [],
};

export const mockUploadTokens: Record<string, ProjectAPIToken[]> = {
  prj_default: [
    { id: "ust_a1", name: "github-actions", prefix: "los_a1b2c3d4", created_at: ago(60 * 24 * 5), last_used_at: ago(60 * 3), revoked: false },
  ],
};

export const mockUsers: ManagedUser[] = [
  { id: "usr_admin", username: "admin", role: "admin", created_at: ago(60 * 24 * 40) },
  { id: "usr_alice", username: "alice", role: "member", created_at: ago(60 * 24 * 18) },
  { id: "usr_bob", username: "bob", role: "member", created_at: ago(60 * 24 * 9) },
  { id: "usr_viewer", username: "viewer", role: "viewer", created_at: ago(60 * 24 * 2) },
];

// ── project ops ──────────────────────────────────────────────────────────────

export function createMockProject(name: string, slug: string): Project {
  const p: Project = { id: nextId("prj"), name, slug, created_at: nowIso() };
  mockProjects.push(p);
  mockTokens[p.id] = [];
  mockUploadTokens[p.id] = [];
  return p;
}

export function updateMockProject(id: string, name: string, slug: string): Project {
  const project = mockProjects.find((item) => item.id === id);
  if (!project) throw new Error("project not found");
  if (name) project.name = name;
  if (slug) project.slug = slug;
  return project;
}

export function deleteMockProject(id: string): void {
  const index = mockProjects.findIndex((project) => project.id === id);
  if (index >= 0) mockProjects.splice(index, 1);
  delete mockTokens[id];
  delete mockUploadTokens[id];
}

export function createMockProjectAPIToken(projectId: string, name: string): IssuedProjectAPIToken {
  const slug = mockProjects.find((p) => p.id === projectId)?.slug ?? "lo";
  const prefix = `lo_${slug.slice(0, 4)}_${hexId(`${projectId}${name}${counter}`, 4)}`;
  const secret = `${prefix}_${hexId(`secret${projectId}${name}${counter++}`, 32)}`;
  const tok: ProjectAPIToken = {
    id: nextId("tok"),
    name,
    prefix,
    created_at: nowIso(),
    last_used_at: null,
    revoked: false,
  };
  (mockTokens[projectId] ??= []).push(tok);
  return { ...tok, value: secret };
}

export function revokeMockProjectAPIToken(projectId: string, tokenId: string): void {
  const list = mockTokens[projectId];
  if (!list) return;
  const token = list.find((item) => item.id === tokenId);
  if (token) token.revoked = true;
}

export function createMockUploadToken(projectId: string, name: string): IssuedProjectAPIToken {
  const prefix = `los_${hexId(`${projectId}${name}${counter}`, 8)}`;
  const secret = `${prefix}${hexId(`secret${projectId}${name}${counter++}`, 40)}`;
  const tok: ProjectAPIToken = {
    id: nextId("ust"),
    name,
    prefix,
    created_at: nowIso(),
    last_used_at: null,
    revoked: false,
  };
  (mockUploadTokens[projectId] ??= []).push(tok);
  return { ...tok, value: secret };
}

export function revokeMockUploadToken(projectId: string, tokenId: string): void {
  const token = mockUploadTokens[projectId]?.find((item) => item.id === tokenId);
  if (token) token.revoked = true;
}

// ── user ops ─────────────────────────────────────────────────────────────────

export function createMockUser(username: string, role: Role): ManagedUser {
  const u: ManagedUser = { id: nextId("usr"), username, role, created_at: nowIso() };
  mockUsers.push(u);
  return u;
}

export function updateMockUserRole(id: string, role: Role): ManagedUser {
  const u = mockUsers.find((x) => x.id === id);
  if (!u) throw new Error("user not found");
  u.role = role;
  return u;
}

export function deleteMockUser(id: string): void {
  const idx = mockUsers.findIndex((x) => x.id === id);
  if (idx >= 0) mockUsers.splice(idx, 1);
}
