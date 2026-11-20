import { expect, test, type Page } from "@playwright/test";

async function openAs(
  page: Page,
  role: "admin" | "member" | "viewer",
  path: string,
): Promise<void> {
  const session = JSON.stringify({ username: role, role });
  await page.addInitScript(
    ({ raw }) => {
      window.localStorage.setItem("lo-language", "zh");
      window.localStorage.setItem("lo-mock-session", raw);
      window.localStorage.setItem("lo-project", "production");
    },
    { raw: session },
  );
  await page.goto(path);
}

test("用户弹窗：新建与重置密码在取消及切换目标后彻底清空密码", async ({ page }) => {
  await openAs(page, "admin", "/settings/users");
  await expect(page.getByRole("cell", { name: "alice" })).toBeVisible();

  // CreateUserDialog: fill password, cancel, reopen -> must be empty.
  await page.getByRole("button", { name: "新建用户" }).click();
  await page.locator("#u-name").fill("tempuser");
  await page.locator("#u-pass").fill("TempPass123!");
  await page.getByRole("button", { name: "取消" }).click();

  await page.getByRole("button", { name: "新建用户" }).click();
  await expect(page.locator("#u-name")).toHaveValue("");
  await expect(page.locator("#u-pass")).toHaveValue("");
  await page.getByRole("button", { name: "取消" }).click();

  // ResetPasswordDialog: type password for alice, cancel, open bob -> must be empty.
  const aliceRow = page.getByRole("row", { name: /alice/ });
  await aliceRow.getByRole("button", { name: "重置密码" }).click();
  await expect(page.getByRole("heading", { name: /重置 “alice” 的密码/ })).toBeVisible();
  await page.locator("#reset-pass").fill("AliceSecret99!");
  await page.getByRole("button", { name: "取消" }).click();

  const bobRow = page.getByRole("row", { name: /bob/ });
  await bobRow.getByRole("button", { name: "重置密码" }).click();
  await expect(page.getByRole("heading", { name: /重置 “bob” 的密码/ })).toBeVisible();
  await expect(page.locator("#reset-pass")).toHaveValue("");
});

test("通知渠道：卡片仅展示脱敏目标且编辑表单不回填密钥", async ({ page }) => {
  await openAs(page, "admin", "/alerts/channels");

  await expect(page.getByText("Ops Webhook")).toBeVisible();
  await expect(page.getByText("https://hooks.example.com/lo/ops")).toBeVisible();
  await expect(page.getByText("[REDACTED]")).toHaveCount(0);

  const opsCard = page.locator("main").locator("div", { hasText: "Ops Webhook" }).first();
  await opsCard.getByRole("button", { name: "编辑" }).first().click();
  await expect(page.locator("#c-url")).toHaveValue("https://hooks.example.com/lo/ops");
  await expect(page.locator("#c-secret")).toHaveValue("");
  await expect(page.locator("#c-secret")).toHaveAttribute("placeholder", "已配置，留空保持不变");
});

test("角色隔离：viewer 不可见受限告警管理页且直达显式提示无权限", async ({ page }) => {
  await openAs(page, "viewer", "/alerts/rules");
  await expect(page.getByText("无访问权限")).toBeVisible();
  await expect(page.getByText("暂无告警规则")).toHaveCount(0);
  await expect(page.locator("aside").getByRole("link", { name: "告警规则" })).toHaveCount(0);
  await expect(page.locator("aside").getByRole("link", { name: "通知渠道" })).toHaveCount(0);

  await page.goto("/alerts/channels");
  await expect(page.getByText("无访问权限")).toBeVisible();
  await expect(page.getByText("暂无通知渠道")).toHaveCount(0);

  await page.goto("/issues");
  await expect(page.locator("main ul li button").first()).toBeVisible();
  await expect(page.locator('main input[type="checkbox"]')).toHaveCount(0);
  await expect(page.getByRole("button", { name: "解决", exact: true })).toHaveCount(0);

  await page.goto("/settings");
  await expect(page.getByText("只读模式：修改项目告警配置需要成员或管理员权限。")).toBeVisible();
});

test("性能：点击刷新推进后端接口查询时间窗", async ({ page }) => {
  await openAs(page, "admin", "/performance");
  await page.getByRole("tab", { name: "后端接口" }).click();
  const firstRow = page.locator('tbody tr[role="link"]').first();
  await expect(firstRow).toBeVisible();

  await firstRow.click();
  await expect(page).toHaveURL(/\/traces\?/);
  const firstEnd = Number(new URL(page.url()).searchParams.get("end"));
  expect(Number.isFinite(firstEnd)).toBe(true);

  await page.goBack();
  await page.getByRole("tab", { name: "后端接口" }).click();
  await expect(firstRow).toBeVisible();
  // Click the Endpoints tab refresh button and wait for the refreshed window data to load.
  const refreshButton = page.getByRole("button", { name: "刷新" }).last();
  await refreshButton.click();
  await expect(refreshButton).toBeDisabled();
  await expect(refreshButton).toBeEnabled();
  await firstRow.click();
  await expect(page).toHaveURL(/\/traces\?/);
  const secondEnd = Number(new URL(page.url()).searchParams.get("end"));
  expect(secondEnd).toBeGreaterThan(firstEnd);
});
