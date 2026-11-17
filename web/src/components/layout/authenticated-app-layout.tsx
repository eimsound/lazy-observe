import { Outlet } from "@tanstack/react-router";
import { ProjectProvider } from "@/lib/project/project-provider";
import { AppShell } from "./app-shell";

export function AuthenticatedAppLayout() {
  return (
    <ProjectProvider>
      <AppShell>
        <Outlet />
      </AppShell>
    </ProjectProvider>
  );
}
