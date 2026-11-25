import { expect, test, type Page } from "@playwright/test";

// Console links from alert notifications: `?project=` selects the project,
// `/alerts?fingerprint=|rule=` narrows the event list.

async function open(page: Page, path: string): Promise<void> {
  await page.addInitScript(() => {
    window.localStorage.setItem("lo-language", "zh");
    window.localStorage.setItem("lo-mock-session", JSON.stringify({ username: "member", role: "member" }));
    window.localStorage.setItem("lo-project", "production");
  });
  await page.goto(path);
}

test("告警链接：?project= 切换到链接项目并从 URL 移除，规则筛选可清除", async ({ page }) => {
  await open(page, "/alerts?project=staging&rule=HighErrorRate");

  const chip = page.getByText("规则：HighErrorRate");
  await expect(chip).toBeVisible();
  await expect(page).toHaveURL(/\/alerts\?rule=HighErrorRate$/);
  await expect.poll(() => page.evaluate(() => window.localStorage.getItem("lo-project"))).toBe("staging");

  const rows = page.locator("tbody tr");
  await expect(rows.first()).toBeVisible();
  for (const text of await rows.allInnerTexts()) {
    expect(text).toContain("HighErrorRate");
  }

  await page.getByRole("button", { name: "清除筛选" }).click();
  await expect(chip).toBeHidden();
  await expect(page).toHaveURL(/\/alerts$/);
});

test("告警链接：已清理的事件指纹显示指纹筛选与清除动作", async ({ page }) => {
  await open(page, "/alerts?fingerprint=deadbeefdeadbeef");

  await expect(page.getByText("指纹：deadbeefdeadbeef")).toBeVisible();
  await expect(page.getByText("暂无告警事件")).toBeVisible();
  await page.getByRole("button", { name: "清除筛选" }).last().click();
  await expect(page.getByText("指纹：deadbeefdeadbeef")).toBeHidden();
  await expect(page.locator("tbody tr").first()).toBeVisible();
});

test("告警链接：不可访问的项目不回落到其他项目", async ({ page }) => {
  await open(page, "/issues/42?project=gone");

  await expect(page.getByText("无法打开项目 gone")).toBeVisible();
  await page.getByRole("button", { name: "打开 生产环境" }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByText("无法打开项目 gone")).toBeHidden();
});

test("告警链接：手动切换项目后清除上一个项目的事件筛选", async ({ page }) => {
  await open(page, "/alerts?rule=HighErrorRate&status=resolved");
  await expect(page.getByText("规则：HighErrorRate")).toBeVisible();

  await page.getByRole("button", { name: "切换项目" }).click();
  await page.getByRole("menuitem", { name: /预发布/ }).click();

  await expect(page).toHaveURL(/\/alerts$/);
  await expect(page.getByText("规则：HighErrorRate")).toBeHidden();
});
