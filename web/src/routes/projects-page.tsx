import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  Copy,
  FolderKanban,
  KeyRound,
  Loader2,
  Lock,
  Pencil,
  Plus,
  Trash2,
} from "lucide-react";
import {
  listProjects,
  createProject,
  updateProject,
  deleteProject,
  listProjectAPITokens,
  createProjectAPIToken,
  revokeProjectAPIToken,
  type Project,
  type IssuedProjectAPIToken,
  type ProjectAPIToken,
} from "@/lib/api/projects";
import { useAuth } from "@/lib/auth/auth-provider";
import { hasCapability } from "@/lib/auth/permissions";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { SnippetBlock } from "@/components/common/snippet-block";
import { SourceMapUploadTokens } from "@/components/projects/source-map-upload-tokens";
import { projectDsn } from "@/lib/tokens/token-usage";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { queryViewState } from "./query-view-state";

const fmt = (iso: string) => (iso ? formatDateTime(dayjs(iso).valueOf()) : "—");

export function ProjectsPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canManage = hasCapability(role, "projects:manage");

  const projects = useQuery({ queryKey: ["projects"], queryFn: ({ signal }) => listProjects(signal) });

  const [createOpen, setCreateOpen] = useState(false);
  const [editingProject, setEditingProject] = useState<Project | null>(null);
  const [tokensProject, setTokensProject] = useState<Project | null>(null);
  const [deletingProject, setDeletingProject] = useState<Project | null>(null);
  const queryClient = useQueryClient();

  const rows = projects.data ?? [];
  const viewState = queryViewState({
    hasData: projects.data !== undefined,
    isLoading: projects.isLoading,
    isError: projects.isError,
    isEmpty: rows.length === 0,
  });

  return (
    <PageContainer>
      <PageActions>
        <ManageGate canManage={canManage}>
          <Button onClick={() => setCreateOpen(true)} disabled={!canManage} className="gap-1.5">
            <Plus className="size-4" />
            {t("projects.newProject")}
          </Button>
        </ManageGate>
      </PageActions>

      {viewState === "stale" && (
        <QueryStaleNotice
          isRetrying={projects.isFetching}
          onRetry={() => void projects.refetch()}
        />
      )}

      <Card>
        <CardContent className="p-0">
          {viewState === "pending" ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : viewState === "error" ? (
            <QueryErrorState error={projects.error} onRetry={() => void projects.refetch()} />
          ) : viewState === "empty" ? (
            <EmptyState
              icon={<FolderKanban className="size-6" />}
              title={t("projects.empty")}
              desc={t("projects.emptyDesc")}
            />
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("projects.colName")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("projects.colSlug")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("projects.colCreated")}
                    </th>
                    <th className="px-4 py-2.5 text-right font-medium">
                      {t("projects.colTokens")}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((p) => (
                    <tr key={p.id} className="border-b border-border/60 last:border-0">
                      <td className="px-4 py-3 font-medium">{p.name}</td>
                      <td className="px-4 py-3">
                        <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
                          {p.slug}
                        </span>
                      </td>
                      <td className="hidden px-4 py-3 text-muted-foreground md:table-cell">
                        {fmt(p.created_at)}
                      </td>
                      <td className="px-4 py-3 text-right">
                        <div className="inline-flex items-center gap-1.5">
                          <Button
                            variant="outline"
                            size="sm"
                            className="gap-1.5"
                            onClick={() => setTokensProject(p)}
                          >
                            <KeyRound className="size-3.5" />
                            {t("projects.manageTokens")}
                          </Button>
                          {canManage && p.slug !== "lazy-observe" && (
                            <>
                              <Button
                                variant="ghost"
                                size="icon"
                                aria-label={t("projects.editProject")}
                                onClick={() => setEditingProject(p)}
                              >
                                <Pencil className="size-4" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                aria-label={t("projects.deleteProject")}
                                onClick={() => setDeletingProject(p)}
                              >
                                <Trash2 className="size-4 text-destructive" />
                              </Button>
                            </>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>

      {createOpen && (
        <CreateProjectDialog key="create-project" open={createOpen} onOpenChange={setCreateOpen} />
      )}
      {editingProject && (
        <UpdateProjectDialog
          key={editingProject.id}
          project={editingProject}
          onOpenChange={(open) => !open && setEditingProject(null)}
        />
      )}
      {tokensProject && (
        <TokensDialog
          key={tokensProject.id}
          project={tokensProject}
          onOpenChange={(o) => !o && setTokensProject(null)}
          canManage={canManage}
        />
      )}
      {deletingProject && (
        <ConfirmDialog
          key={deletingProject.id}
          open
          onOpenChange={(open) => !open && setDeletingProject(null)}
          title={t("projects.deleteConfirm", { name: deletingProject.name })}
          description={t("projects.deleteConfirmDesc")}
          confirmLabel={t("projects.deleteProject")}
          onConfirm={async () => {
            await deleteProject(deletingProject.id);
            if (tokensProject?.id === deletingProject.id) setTokensProject(null);
            await queryClient.invalidateQueries({ queryKey: ["projects"] });
            setDeletingProject(null);
          }}
        />
      )}
    </PageContainer>
  );
}

function UpdateProjectDialog({
  project,
  onOpenChange,
}: {
  project: Project;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [name, setName] = useState(project.name);
  const slug = project.slug;
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => updateProject(project.id, name.trim(), slug.trim()),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("projects.updateError")),
  });

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("projects.editProject")}</DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="edit-p-name">{t("projects.projectName")}</Label>
            <Input
              id="edit-p-name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              autoFocus
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="edit-p-slug">{t("projects.slug")}</Label>
            <Input
              id="edit-p-slug"
              value={slug}
              className="font-mono text-sm"
              spellCheck={false}
              disabled
            />
            <p className="text-xs text-muted-foreground">{t("projects.slugImmutable")}</p>
          </div>
          {error && (
            <p className="text-sm text-destructive" role="alert">
              {error}
            </p>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={mutation.isPending}>
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => mutation.mutate()}
            disabled={!name.trim() || !slug.trim() || mutation.isPending}
          >
            {mutation.isPending && <Loader2 className="size-4 animate-spin" />}
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Wraps a management control with an "admin required" tooltip when disabled. */
function ManageGate({ canManage, children }: { canManage: boolean; children: React.ReactNode }) {
  const { t } = useTranslation();
  if (canManage) return <>{children}</>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex cursor-not-allowed">{children}</span>
      </TooltipTrigger>
      <TooltipContent>{t("projects.adminOnly")}</TooltipContent>
    </Tooltip>
  );
}

function CreateProjectDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => createProject(name.trim(), slug.trim()),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("projects.createError")),
  });

  const slugify = (v: string) =>
    v.toLowerCase().trim().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "");

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("projects.newProject")}</DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="p-name">{t("projects.projectName")}</Label>
            <Input
              id="p-name"
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                if (!slug) setSlug(slugify(e.target.value));
              }}
              placeholder={t("projects.projectNamePlaceholder")}
              autoFocus
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="p-slug">{t("projects.slug")}</Label>
            <Input
              id="p-slug"
              value={slug}
              onChange={(e) => setSlug(slugify(e.target.value))}
              placeholder={t("projects.slugPlaceholder")}
              className="font-mono text-sm"
              spellCheck={false}
            />
            <p className="text-xs text-muted-foreground">{t("projects.slugHint")}</p>
          </div>
          {error && (
            <p className="text-sm text-destructive" role="alert">
              {error}
            </p>
          )}
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={mutation.isPending}
          >
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => mutation.mutate()}
            disabled={!name.trim() || !slug.trim() || mutation.isPending}
          >
            {mutation.isPending && <Loader2 className="size-4 animate-spin" />}
            {t("common.create")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function TokensDialog({
  project,
  onOpenChange,
  canManage,
}: {
  project: Project;
  onOpenChange: (open: boolean) => void;
  canManage: boolean;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const projectId = project.id;

  const tokens = useQuery({
    queryKey: ["project-api-tokens", projectId],
    queryFn: ({ signal }) => listProjectAPITokens(projectId, signal),
    retry: false,
  });

  const [name, setName] = useState("");
  const [createdToken, setCreatedToken] = useState<IssuedProjectAPIToken | null>(null);
  const [copied, setCopied] = useState(false);
  const [revoking, setRevoking] = useState<ProjectAPIToken | null>(null);
  const [error, setError] = useState<string | null>(null);

  const issue = useMutation({
    mutationFn: () => createProjectAPIToken(projectId, name.trim()),
    onSuccess: (tok) => {
      setCreatedToken(tok);
      setName("");
      setCopied(false);
      void queryClient.invalidateQueries({ queryKey: ["project-api-tokens", projectId] });
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
    onError: (e) => setError((e as Error).message),
  });

  async function copy(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable */
    }
  }

  const tokenRows = tokens.data ?? [];
  const tokenState = queryViewState({
    hasData: tokens.data !== undefined,
    isLoading: tokens.isLoading,
    isError: tokens.isError,
    isEmpty: tokenRows.length === 0,
  });

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <KeyRound className="size-4" />
            {t("projects.tokensFor", { name: project.name })}
          </DialogTitle>
        </DialogHeader>

        {/* ADR 0010: a project API token is ingest-only; reads need a user token. */}
        <div className="rounded-lg border border-border bg-muted/40 p-3 text-xs">
          <p className="font-medium text-foreground">{t("projects.ingestOnlyTitle")}</p>
          <p className="mt-1 text-muted-foreground">{t("projects.ingestOnlyDesc")}</p>
        </div>

        {/* Freshly-created secret (shown once) */}
        {createdToken && (
          <div className="space-y-2 rounded-lg border border-primary/40 bg-primary/5 p-3">
            <div className="flex items-center gap-2 text-sm font-medium">
              <Check className="size-4 text-primary" />
              {t("projects.createdTitle")}
            </div>
            <p className="text-xs text-muted-foreground">{t("projects.createdDesc")}</p>
            <div className="flex items-center gap-2">
              <code className="flex-1 truncate rounded border border-border bg-background px-2 py-1.5 font-mono text-xs">
                {createdToken.value}
              </code>
              <Button size="sm" variant="outline" className="gap-1.5" onClick={() => copy(createdToken.value)}>
                {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                {copied ? t("common.copied") : t("common.copy")}
              </Button>
            </div>
            <p className="text-[11px] text-muted-foreground">{t("projects.copyWarning")}</p>
            <SnippetBlock
              label={t("projects.dsnLabel")}
              value={projectDsn(window.location.origin, project.slug, createdToken.value)}
              hint={t("projects.dsnHint")}
            />
          </div>
        )}

        {/* Issue form */}
        {canManage && (
          <div className="flex items-end gap-2">
            <div className="flex-1 space-y-1.5">
              <Label htmlFor="tok-name">{t("projects.tokenName")}</Label>
              <Input
                id="tok-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={t("projects.tokenNamePlaceholder")}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && name.trim()) issue.mutate();
                }}
              />
            </div>
            <Button onClick={() => issue.mutate()} disabled={!name.trim() || issue.isPending} className="gap-1.5">
              {issue.isPending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
              {t("projects.issueToken")}
            </Button>
          </div>
        )}
        {error && (
          <p className="text-sm text-destructive" role="alert">
            {error}
          </p>
        )}

        {tokenState === "stale" && (
          <QueryStaleNotice
            isRetrying={tokens.isFetching}
            onRetry={() => void tokens.refetch()}
          />
        )}

        {/* Token list */}
        <div className="rounded-lg border border-border">
          {tokenState === "pending" ? (
            <div className="space-y-2 p-3">
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
            </div>
          ) : tokenState === "error" ? (
            <QueryErrorState error={tokens.error} onRetry={() => void tokens.refetch()} />
          ) : tokenState === "empty" ? (
            <p className="px-3 py-6 text-center text-sm text-muted-foreground">
              {t("projects.noTokens")}
            </p>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                  <th className="px-3 py-2 font-medium">{t("projects.tokenColName")}</th>
                  <th className="px-3 py-2 font-medium">{t("projects.tokenColPrefix")}</th>
                  <th className="hidden px-3 py-2 font-medium sm:table-cell">
                    {t("projects.tokenColLastUsed")}
                  </th>
                  {canManage && <th className="px-3 py-2" />}
                </tr>
              </thead>
              <tbody>
                {tokenRows.map((tok) => (
                  <tr key={tok.id} className="border-b border-border/60 last:border-0">
                    <td className="px-3 py-2 font-medium">
                      <span className={tok.revoked ? "text-muted-foreground line-through" : ""}>
                        {tok.name}
                      </span>
                      {tok.revoked && (
                        <span className="ml-1.5 text-[11px] font-normal text-muted-foreground">
                          {t("projects.revoked")}
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-2">
                      <span className="font-mono text-xs text-muted-foreground">{tok.prefix}…</span>
                    </td>
                    <td className="hidden px-3 py-2 text-xs text-muted-foreground sm:table-cell">
                      {tok.last_used_at ? fmt(tok.last_used_at) : t("projects.neverUsed")}
                    </td>
                    {canManage && (
                      <td className="px-3 py-2 text-right">
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={t("projects.revokeToken")}
                          onClick={() => setRevoking(tok)}
                          disabled={tok.revoked}
                        >
                          <Trash2 className="size-4 text-destructive" />
                        </Button>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        {canManage && <SourceMapUploadTokens key={project.id} project={project} />}

        {!canManage && (
          <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <Lock className="size-3.5" />
            {t("projects.adminOnly")}
          </p>
        )}
      </DialogContent>

      {revoking && (
        <ConfirmDialog
          key={revoking.id}
          open
          onOpenChange={(o) => !o && setRevoking(null)}
          title={t("projects.revokeConfirm", { name: revoking.name })}
          description={t("projects.revokeConfirmDesc")}
          confirmLabel={t("projects.revokeToken")}
          onConfirm={async () => {
            await revokeProjectAPIToken(projectId, revoking.id);
            void queryClient.invalidateQueries({ queryKey: ["project-api-tokens", projectId] });
            void queryClient.invalidateQueries({ queryKey: ["projects"] });
            setRevoking(null);
          }}
        />
      )}
    </Dialog>
  );
}
