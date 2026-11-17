import { expect, test } from "@playwright/test";

test("管理员登录后返回原受保护页面", async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem("lo-language", "zh");
  });

  await page.goto("/settings/users?source=e2e");

  await expect(page).toHaveURL(
    /\/login\?redirect=%2Fsettings%2Fusers%3Fsource%3De2e$/,
  );
  await expect(page.locator("#username")).toHaveValue("admin");
  await expect(page.locator("#password")).toHaveValue("demo");

  await page.locator('button[type="submit"]').click();

  await expect(page).toHaveURL("/settings/users?source=e2e");
  await expect(page.getByRole("cell", { name: /admin/ })).toBeVisible();
  await expect(page.getByRole("button", { name: "用户菜单" })).toBeVisible();
});
