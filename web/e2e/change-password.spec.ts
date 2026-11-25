import { expect, test, type Page } from "@playwright/test";

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

async function seedFlaggedAdmin(page: Page): Promise<void> {
  await page.addInitScript(
    ({ session }) => {
      window.localStorage.setItem("lo-language", "zh");
      if (!window.localStorage.getItem("lo-mock-session")) {
        window.localStorage.setItem("lo-mock-session", session);
        window.localStorage.setItem("lo-project", "production");
      }
    },
    { session: flaggedAdmin },
  );
}

async function changeInitialPassword(page: Page): Promise<void> {
  await expect(page.getByRole("heading", { name: "请先修改初始密码" })).toBeVisible();
  await page.getByLabel("当前密码").fill("generated-initial-password");
  await page.getByLabel("新密码", { exact: true }).fill("operator-chosen-password");
  await page.getByLabel("确认新密码").fill("operator-chosen-password");
  await page.getByRole("button", { name: "修改密码" }).click();
}

// #361: the post-change redirect must keep the query string (and hash) out of
// the route path: no `$` template interpolation, no `//` folding, no trailing
// slash trimming inside query values.
test("改密后告警深链接的查询参数与锚点原样保留", async ({ page }) => {
  await seedFlaggedAdmin(page);
  const rule = "foo/$bar//baz/";
  await page.goto(`/alerts?project=staging&rule=${encodeURIComponent(rule)}#events`);

  await expect(page).toHaveURL(/\/change-password\?redirect=/);
  await changeInitialPassword(page);

  await expect(page.getByText(`规则：${rule}`)).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.localStorage.getItem("lo-project"))).toBe("staging");
  const url = new URL(page.url());
  expect(url.pathname).toBe("/alerts");
  expect(url.searchParams.get("rule")).toBe(rule);
  expect(url.hash).toBe("#events");
});

// A redirect carried verbatim (e.g. a bookmarked or hand-built
// /change-password link) keeps raw `$`, `//` and a trailing `/` in the query.
test("改密后原样携带的告警深链接查询参数不被路由模板和路径规范化改写", async ({ page }) => {
  await seedFlaggedAdmin(page);
  const rule = "foo/$bar//baz/";
  await page.goto(`/change-password?redirect=${encodeURIComponent(`/alerts?project=staging&rule=${rule}`)}`);

  await changeInitialPassword(page);

  await expect(page.getByText(`规则：${rule}`)).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.localStorage.getItem("lo-project"))).toBe("staging");
  const url = new URL(page.url());
  expect(url.pathname).toBe("/alerts");
  expect(url.searchParams.get("rule")).toBe(rule);
});

test("改密后动态路由深链接不把查询串并入路径参数", async ({ page }) => {
  await seedFlaggedAdmin(page);
  await page.goto(`/change-password?redirect=${encodeURIComponent("/issues/42?project=staging&event=$evt/")}`);

  await changeInitialPassword(page);

  await expect.poll(() => new URL(page.url()).pathname).toBe("/issues/42");
  await expect.poll(() => page.evaluate(() => window.localStorage.getItem("lo-project"))).toBe("staging");
  expect(new URL(page.url()).searchParams.get("event")).toBe("$evt/");
});
