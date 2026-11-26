import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation } from "@tanstack/react-query";
import { KeyRound, Loader2 } from "lucide-react";
import { changePassword } from "@/lib/api/auth";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export function PasswordCard({ onChanged }: { onChanged?: () => void | Promise<void> } = {}) {
  const { t } = useTranslation();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const mutation = useMutation({
    mutationFn: () => changePassword(current, next),
    onSuccess: () => {
      setCurrent("");
      setNext("");
      setConfirm("");
      setError(null);
      setDone(true);
      setTimeout(() => setDone(false), 2500);
      void onChanged?.();
    },
    onError: (cause) => setError((cause as Error).message),
  });

  const mismatch = confirm !== "" && next !== confirm;
  const tooShort = next !== "" && next.length < 8;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-4" />
          {t("password.title")}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="pw-current">{t("password.current")}</Label>
          <Input
            id="pw-current"
            type="password"
            value={current}
            onChange={(event) => setCurrent(event.target.value)}
            autoComplete="current-password"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="pw-next">{t("password.new")}</Label>
          <Input
            id="pw-next"
            type="password"
            value={next}
            onChange={(event) => setNext(event.target.value)}
            autoComplete="new-password"
          />
          {tooShort && <p className="text-xs text-destructive">{t("password.tooShort")}</p>}
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="pw-confirm">{t("password.confirm")}</Label>
          <Input
            id="pw-confirm"
            type="password"
            value={confirm}
            onChange={(event) => setConfirm(event.target.value)}
            autoComplete="new-password"
          />
          {mismatch && <p className="text-xs text-destructive">{t("password.mismatch")}</p>}
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
        {done && <p className="text-sm text-primary">{t("password.changed")}</p>}
        <Button
          onClick={() => mutation.mutate()}
          disabled={!current || !next || next.length < 8 || next !== confirm || mutation.isPending}
          className="gap-1.5"
        >
          {mutation.isPending && <Loader2 className="size-4 animate-spin" />}
          {t("password.submit")}
        </Button>
      </CardContent>
    </Card>
  );
}
