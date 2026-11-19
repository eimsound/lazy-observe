import { expect, test, type Page } from "@playwright/test";

/**
 * ADR 0010: a project API token only ingests — the UI must say so and hand out
 * the SDK DSN — while reads (query API, MCP, Grafana) use a personal API token
 * with `X-LO-Project`. These specs pin the snippets each issuance surfaces.
 */

const adminSession = JSON.stringify({ username: "admin", role: "admin" });

async function openAsAdmin(page: Page, path: string): Promise<void> {
  await page.addInitScript(
    ({ session }) => {
      window.localStorage.setItem("lo-language", "zh");
      window.localStorage.setItem("lo-mock-session", session);
      // Pin the read scope so the snippets do not depend on auto-selection.
      window.localStorage.setItem("lo-project", "production");
    },
    { session: adminSession },
  );
  await page.goto(path);
}

/** The origin the snippets interpolate, taken from the live page URL. */
function originOf(page: Page): string {
  return new URL(page.url()).origin;
}

const escapeRegExp = (value: string) => value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

test("项目 Token 签发后才出现仅摄取说明与 DSN 片段", async ({ page }) => {
  await openAsAdmin(page, "/settings/projects");
  const origin = originOf(page);

  await page.getByRole("button", { name: "管理令牌" }).first().click();
  await expect(page.getByText("仅摄取（ingest only）")).toBeVisible();
  // The DSN requires a token value, so it must not exist before issuance.
  await expect(page.getByText(new RegExp(`/p/production@lo_`))).toHaveCount(0);

  await page.locator("#tok-name").fill("ingest-prod");
  await page.getByRole("button", { name: "创建 API Token" }).click();

  await expect(page.getByText(new RegExp(`${escapeRegExp(origin)}/p/production@lo_`))).toBeVisible();
  await expect(page.getByText(/查询、MCP 或 Grafana/)).toBeVisible();
});

test("个人 Token 签发后给出 MCP 命令与带 X-LO-Project 的 Grafana 片段", async ({ page }) => {
  await openAsAdmin(page, "/settings");
  const origin = originOf(page);

  await page.locator("#user-token-name").fill("local-cli");
  await page.getByRole("button", { name: "创建 Token" }).click();

  await expect(
    page.getByText(new RegExp(`lazy-observe mcp --dsn '${escapeRegExp(origin)}/p/production@lou_`)),
  ).toBeVisible();
  // The Grafana data source is the gateway root itself, with no path prefix.
  await expect(page.getByText(origin, { exact: true })).toBeVisible();
  await expect(page.getByText("X-LO-Project: production")).toBeVisible();
  await expect(page.getByText("Authorization: Bearer lou_", { exact: false })).toBeVisible();
});

test("Source Map 上传 Token：创建后只显示一次并给出 los_ DSN，吊销后置灰", async ({ page }) => {
  await openAsAdmin(page, "/settings/projects");
  const origin = originOf(page);

  await page.getByRole("button", { name: "管理令牌" }).first().click();
  // The ingest-only note points to the upload token for source maps.
  await expect(page.getByText(/上传 Source Map 请改用 Source Map 上传 Token/)).toBeVisible();
  await expect(page.getByText("Source Map 上传 Token", { exact: true })).toBeVisible();
  await expect(page.getByText(/\/p\/production@los_/)).toHaveCount(0);

  await page.locator("#sm-upload-name").fill("github-actions-2");
  await page.getByRole("button", { name: "创建上传 Token" }).click();

  await expect(page.getByText("Source Map 上传 Token 已创建")).toBeVisible();
  await expect(page.getByText(new RegExp(`${escapeRegExp(origin)}/p/production@los_`))).toBeVisible();
  await expect(page.getByText(/放进 CI secret/).first()).toBeVisible();

  // Revoke the token created above (the seeded one is listed first).
  const row = page.getByRole("row", { name: /github-actions-2/ });
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: "吊销上传 Token" }).click();
  await page.getByRole("button", { name: "吊销上传 Token" }).last().click();
  await expect(page.getByRole("row", { name: /github-actions-2.*已吊销/ })).toBeVisible();
});

test("非管理员看不到 Source Map 上传 Token 管理", async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem("lo-language", "zh");
    window.localStorage.setItem("lo-mock-session", JSON.stringify({ username: "viewer", role: "viewer" }));
    window.localStorage.setItem("lo-project", "production");
  });
  await page.goto("/settings/projects");
  await page.getByRole("button", { name: "管理令牌" }).first().click();
  await expect(page.getByText("仅摄取（ingest only）")).toBeVisible();
  await expect(page.locator("#sm-upload-name")).toHaveCount(0);
});
