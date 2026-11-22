import { expect, test, type Page } from "@playwright/test";

const mockAdmin = JSON.stringify({ username: "admin", role: "admin" });

async function initMockSession(page: Page): Promise<void> {
  await page.addInitScript(
    ({ session }) => {
      window.localStorage.setItem("lo-language", "zh");
      window.localStorage.setItem("lo-mock-session", session);
      window.localStorage.setItem("lo-project", "production");
    },
    { session: mockAdmin },
  );
}

test("Trace 详情：最近 30 秒链路展示写入中提示，过期后自动隐藏，历史链路不展示", async ({
  page,
}) => {
  const recentTraceId = "11111111111111111111111111111111";
  const oldTraceId = "22222222222222222222222222222222";

  await page.clock.install();
  await initMockSession(page);
  await page.addInitScript(
    ({ recentId, oldId }) => {
      (
        globalThis as { __loMockTraceAgeMsById?: Record<string, number> }
      ).__loMockTraceAgeMsById = {
        [recentId]: 5_000,
        [oldId]: 120_000,
      };
    },
    { recentId: recentTraceId, oldId: oldTraceId },
  );

  await page.goto(`/traces/${recentTraceId}`);
  await expect(page.locator("main")).toContainText(new RegExp(`Trace ID:${recentTraceId}`));

  const notice = page.getByTestId("recent-ingest-notice");
  await expect(notice).toBeVisible();
  await expect(notice).toContainText("最近 30 秒数据可能仍在写入中");
  await expect(notice).toContainText("latencyOffset=0s");

  await notice.getByRole("button", { name: /^Refresh$|^刷新$/ }).click();
  await expect(notice).toBeVisible();

  await page.clock.fastForward(31_000);
  await expect(notice).toHaveCount(0);

  await page.goto(`/traces/${oldTraceId}`);
  await expect(page.locator("main")).toContainText(new RegExp(`Trace ID:${oldTraceId}`));
  await expect(page.getByTestId("recent-ingest-notice")).toHaveCount(0);
});

test("LLM 面板：默认近实时时间窗展示写入中提示，30 秒后隐藏，刷新推进时间窗后重新显示", async ({
  page,
}) => {
  await page.clock.install();
  await initMockSession(page);

  await page.goto("/llm");
  await expect(page.getByText("调用总数")).toBeVisible();

  const notice = page.getByTestId("recent-ingest-notice");
  await expect(notice).toBeVisible();
  await expect(notice).toContainText("最近 30 秒数据可能仍在写入中");

  await notice.getByRole("button", { name: /^Refresh$|^刷新$/ }).click();
  await expect(page.getByText("调用总数")).toBeVisible();
  await expect(notice).toBeVisible();

  await page.clock.fastForward(31_000);
  await expect(notice).toHaveCount(0);

  await page.getByRole("button", { name: /^Refresh$|^刷新$/ }).click();
  await expect(notice).toBeVisible();
});

test("LLM 面板：空态下仍展示最近 30 秒写入中提示", async ({ page }) => {
  await initMockSession(page);
  await page.addInitScript(() => {
    (globalThis as { __loMockLLMEmpty?: boolean }).__loMockLLMEmpty = true;
  });

  await page.goto("/llm");
  await expect(page.getByText("暂无 LLM 调用")).toBeVisible();
  await expect(page.getByTestId("recent-ingest-notice")).toBeVisible();
});
