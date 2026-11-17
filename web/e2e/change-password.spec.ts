import { expect, test } from "@playwright/test";

// The generated bootstrap admin password carries must_change_password: every
// app route must funnel into the forced change page until it is replaced.
const flaggedAdmin = JSON.stringify({
  username: "admin",
  role: "admin",
  must_change_password: true,
});

test("首次登录的初始密码必须先修改，修改后返回原页面", async ({ page }) => {
  await page.addInitScript(
    ({ session }) => {
      window.localStorage.setItem("lo-language", "zh");
      // Seed once: later reloads must observe the mock password change.
      if (!window.localStorage.getItem("lo-mock-session")) {
        window.localStorage.setItem("lo-mock-session", session);
      }
    },
    { session: flaggedAdmin },
  );

  await page.goto("/settings/users?source=e2e");

  await expect(page).toHaveURL(
    /\/change-password\?redirect=%2Fsettings%2Fusers%3Fsource%3De2e$/,
  );
  await expect(page.getByRole("heading", { name: "请先修改初始密码" })).toBeVisible();
  await expect(page.getByRole("button", { name: "用户菜单" })).toHaveCount(0);

  const submit = page.getByRole("button", { name: "修改密码" });
  await page.getByLabel("当前密码").fill("generated-initial-password");
  await page.getByLabel("新密码", { exact: true }).fill("short");
  await expect(page.getByText("至少 8 个字符")).toBeVisible();
  await expect(submit).toBeDisabled();

  await page.getByLabel("新密码", { exact: true }).fill("operator-chosen-password");
  await page.getByLabel("确认新密码").fill("operator-chosen-password");
  await submit.click();

  await expect(page).toHaveURL("/settings/users?source=e2e");
  await expect(page.getByRole("button", { name: "用户菜单" })).toBeVisible();

  await page.reload();
  await expect(page).toHaveURL("/settings/users?source=e2e");
});

test("未被标记的会话直接访问改密页会被送回原页面", async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem("lo-language", "zh");
    window.localStorage.setItem("lo-mock-session", JSON.stringify({ username: "admin", role: "admin" }));
  });

  await page.goto("/change-password?redirect=%2Fsettings%2Fusers");

  await expect(page).toHaveURL("/settings/users");
});
