import { PageContainer } from "@/components/layout/page";
import { AlertConfigCard } from "@/components/settings/alert-config-card";
import { PasswordCard } from "@/components/settings/password-card";
import { ProfileCard } from "@/components/settings/profile-card";
import { SessionsCard } from "@/components/settings/sessions-card";
import { UserAPITokensCard } from "@/components/settings/user-api-tokens-card";

export function SettingsPage() {
  return (
    <PageContainer className="max-w-3xl space-y-6">
      <ProfileCard />
      <PasswordCard />
      <SessionsCard />
      <UserAPITokensCard />
      <AlertConfigCard />
    </PageContainer>
  );
}
