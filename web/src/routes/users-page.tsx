import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Loader2, Plus, ShieldAlert, Trash2, Users as UsersIcon } from "lucide-react";
import {
  listUsers,
  createUser,
  updateUserRole,
  resetUserPassword,
  deleteUser,
  type ManagedUser,
} from "@/lib/api/users";
import { ROLES, type Role } from "@/lib/api/auth";
import { useAuth } from "@/lib/auth/auth-provider";
import { hasCapability } from "@/lib/auth/permissions";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { formatErrorMessage } from "@/lib/api/errors";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import { queryViewState } from "./query-view-state";

const fmt = (iso: string) => (iso ? formatDateTime(dayjs(iso).valueOf()) : "—");

export function UsersPage() {
  const { t } = useTranslation();
  const { role, user } = useAuth();
  const canManage = hasCapability(role, "users:manage");
  const queryClient = useQueryClient();

  const users = useQuery({
    queryKey: ["users"],
    queryFn: ({ signal }) => listUsers(signal),
    enabled: canManage,
    retry: false,
  });

  const [createOpen, setCreateOpen] = useState(false);
  const [deleting, setDeleting] = useState<ManagedUser | null>(null);
  const [resetting, setResetting] = useState<ManagedUser | null>(null);
  const [roleError, setRoleError] = useState<string | null>(null);

  const roleMutation = useMutation({
    mutationFn: ({ id, newRole }: { id: string; newRole: Role }) => updateUserRole(id, newRole),
    onMutate: () => setRoleError(null),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["users"] }),
    onError: (err) => setRoleError(formatErrorMessage(err, t("common.loadFailed"))),
  });

  // Admin-only page: block non-admins with a clear message.
  if (!canManage) {
    return (
      <PageContainer>
        <Card>
          <CardContent>
            <EmptyState
              icon={<ShieldAlert className="size-6" />}
              title={t("users.forbiddenTitle")}
              desc={t("users.forbiddenDesc")}
            />
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  const rows = users.data ?? [];
  const viewState = queryViewState({
    hasData: users.data !== undefined,
    isLoading: users.isLoading,
    isError: users.isError,
    isEmpty: rows.length === 0,
  });

  return (
    <PageContainer>
      <PageActions>
        <Button onClick={() => setCreateOpen(true)} className="gap-1.5">
          <Plus className="size-4" />
          {t("users.newUser")}
        </Button>
      </PageActions>

      {roleError && (
        <p className="mb-3 text-sm text-destructive" role="alert">
          {roleError}
        </p>
      )}

      {viewState === "stale" && (
        <QueryStaleNotice
          isRetrying={users.isFetching}
          onRetry={() => void users.refetch()}
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
            <QueryErrorState
              error={users.error}
              forbiddenTitle={t("users.forbiddenTitle")}
              forbiddenDesc={t("users.forbiddenDesc")}
              onRetry={() => void users.refetch()}
            />
          ) : viewState === "empty" ? (
            <EmptyState icon={<UsersIcon className="size-6" />} title={t("users.empty")} />
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("users.colUser")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("users.colRole")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("users.colCreated")}
                    </th>
                    <th className="px-4 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((u) => {
                    const isSelf = u.username === user?.username;
                    return (
                      <tr key={u.id} className="border-b border-border/60 last:border-0">
                        <td className="px-4 py-3 font-medium">
                          <span>{u.username}</span>
                          {u.display_name && u.display_name !== u.username && (
                            <span className="ml-2 text-xs font-normal text-muted-foreground">
                              {u.display_name}
                            </span>
                          )}
                          {isSelf && (
                            <span className="ml-2 rounded border border-border bg-muted/50 px-1.5 py-0.5 text-[11px] font-normal text-muted-foreground">
                              {t("users.you")}
                            </span>
                          )}
                        </td>
                        <td className="px-4 py-3">
                          <Select
                            value={u.role}
                            onValueChange={(v) => roleMutation.mutate({ id: u.id, newRole: v as Role })}
                            disabled={isSelf || roleMutation.isPending}
                          >
                            <SelectTrigger className="h-8 w-32" aria-label={t("users.changeRole")}>
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent>
                              {ROLES.map((r) => (
                                <SelectItem key={r} value={r}>
                                  {t(`roles.${r}`)}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        </td>
                        <td className="hidden px-4 py-3 text-muted-foreground md:table-cell">
                          {fmt(u.created_at)}
                        </td>
                        <td className="px-4 py-3 text-right">
                          <Button
                            variant="ghost"
                            size="icon"
                            aria-label={t("users.resetPassword")}
                            disabled={isSelf}
                            title={isSelf ? t("users.cannotEditSelf") : undefined}
                            onClick={() => setResetting(u)}
                          >
                            <KeyRound className="size-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            aria-label={t("users.deleteUser")}
                            disabled={isSelf}
                            title={isSelf ? t("users.cannotEditSelf") : undefined}
                            onClick={() => setDeleting(u)}
                          >
                            <Trash2 className="size-4 text-destructive" />
                          </Button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>

      {createOpen && <CreateUserDialog key="create-user" open={createOpen} onOpenChange={setCreateOpen} />}
      {resetting && (
        <ResetPasswordDialog key={resetting.id} user={resetting} onOpenChange={setResetting} />
      )}
      {deleting && (
        <ConfirmDialog
          key={deleting.id}
          open
          onOpenChange={(o) => !o && setDeleting(null)}
          title={t("users.deleteConfirm", { name: deleting.username })}
          description={t("users.deleteConfirmDesc")}
          confirmLabel={t("users.deleteUser")}
          onConfirm={async () => {
            await deleteUser(deleting.id);
            void queryClient.invalidateQueries({ queryKey: ["users"] });
            setDeleting(null);
          }}
        />
      )}
    </PageContainer>
  );
}

function CreateUserDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [roleValue, setRoleValue] = useState<Role>("member");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () =>
      createUser(username.trim(), password, roleValue, {
        display_name: displayName.trim() || undefined,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["users"] });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("users.createError")),
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("users.newUser")}</DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="u-name">{t("users.username")}</Label>
            <Input
              id="u-name"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder={t("users.usernamePlaceholder")}
              autoComplete="off"
              autoFocus
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="u-display-name">
              {t("profile.displayName")}
              <span className="ml-1 text-xs font-normal text-muted-foreground">
                ({t("common.optional")})
              </span>
            </Label>
            <Input
              id="u-display-name"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
              placeholder={username || t("users.usernamePlaceholder")}
              autoComplete="off"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="u-pass">{t("users.password")}</Label>
            <Input
              id="u-pass"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder={t("users.passwordPlaceholder")}
              autoComplete="new-password"
            />
          </div>
          <div className="space-y-1.5">
            <Label>{t("users.role")}</Label>
            <Select value={roleValue} onValueChange={(v) => setRoleValue(v as Role)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ROLES.map((r) => (
                  <SelectItem key={r} value={r}>
                    {t(`roles.${r}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
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
            disabled={!username.trim() || !password || mutation.isPending}
          >
            {mutation.isPending && <Loader2 className="size-4 animate-spin" />}
            {t("common.create")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ResetPasswordDialog({
  user,
  onOpenChange,
}: {
  user: ManagedUser;
  onOpenChange: (user: ManagedUser | null) => void;
}) {
  const { t } = useTranslation();
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => resetUserPassword(user.id, password),
    onSuccess: () => onOpenChange(null),
    onError: (e) => setError((e as Error).message || t("users.resetError")),
  });

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onOpenChange(null);
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("users.resetTitle", { name: user.username })}</DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="reset-pass">{t("users.password")}</Label>
            <Input
              id="reset-pass"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder={t("users.passwordPlaceholder")}
              autoComplete="new-password"
              autoFocus
            />
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
            onClick={() => onOpenChange(null)}
            disabled={mutation.isPending}
          >
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => mutation.mutate()}
            disabled={password.length < 8 || mutation.isPending}
          >
            {mutation.isPending && <Loader2 className="size-4 animate-spin" />}
            {t("users.resetPassword")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
