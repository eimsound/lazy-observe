import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Loader2, Mail, Save, UserRound } from "lucide-react";
import { updateMe } from "@/lib/api/auth";
import { useAuth } from "@/lib/auth/auth-provider";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export function ProfileCard() {
  const { t } = useTranslation();
  const { user, refresh } = useAuth();
  const queryClient = useQueryClient();
  const [displayName, setDisplayName] = useState(user?.display_name ?? "");
  const [email, setEmail] = useState(user?.email ?? "");
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const mutation = useMutation({
    mutationFn: () =>
      updateMe({ display_name: displayName.trim(), email: email.trim() }),
    onSuccess: async () => {
      setError(null);
      setSaved(true);
      await refresh();
      void queryClient.invalidateQueries({ queryKey: ["users"] });
      setTimeout(() => setSaved(false), 2000);
    },
    onError: (cause) => setError((cause as Error).message),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <UserRound className="size-4" />
          {t("profile.title")}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="profile-display-name">{t("profile.displayName")}</Label>
          <Input
            id="profile-display-name"
            value={displayName}
            onChange={(event) => setDisplayName(event.target.value)}
            placeholder={user?.username ?? ""}
            autoComplete="off"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="profile-email">{t("profile.email")}</Label>
          <div className="relative">
            <Mail className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              id="profile-email"
              type="email"
              className="pl-9"
              value={email}
              onChange={(event) => setEmail(event.target.value)}
              placeholder={t("profile.emailPlaceholder")}
              autoComplete="off"
            />
          </div>
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
        <Button
          onClick={() => mutation.mutate()}
          disabled={mutation.isPending || saved}
          className="gap-1.5"
        >
          {mutation.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
          {saved ? t("profile.saved") : t("profile.save")}
        </Button>
      </CardContent>
    </Card>
  );
}
