import { expect, test, type Page } from "@playwright/test";

const mockAdmin = JSON.stringify({ username: "admin", role: "admin" });

async function openFeature(page: Page, path: string): Promise<void> {
  await page.addInitScript(
    ({ session }) => {
      window.localStorage.setItem("lo-language", "zh");
      window.localStorage.setItem("lo-mock-session", session);
    },
    { session: mockAdmin },
  );
  await page.goto(path);
  await page.locator("main > *").first().waitFor();
  await expect(page.locator("main h1")).toHaveCount(0);
}

test("指标：执行 PromQL 并在图表与表格间切换", async ({ page }) => {
  await openFeature(page, "/metrics?q=up");

  await expect(page.getByLabel("PromQL 表达式")).toContainText("up");
  await page.getByRole("button", { name: "执行查询" }).click();
  await expect(page.getByText(/加载耗时：\d+ ms\u3000结果序列：\d+/)).toBeVisible();

  await page.getByRole("tab", { name: "表格" }).click();
  await expect(page.getByRole("columnheader", { name: "指标" })).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "值" })).toBeVisible();
});

test("指标：可视化构建器与 CodeMirror 双向同步并保护复杂表达式", async ({ page }) => {
  await openFeature(page, "/metrics");

  await page.getByRole("button", { name: "构建器" }).click();
  await page.getByRole("button", { name: "展开指标候选" }).click();
  await page.getByRole("option", { name: "http.server.request.duration_count" }).click();

  const editor = page.getByLabel("PromQL 表达式");
  await expect(editor).toContainText("http.server.request.duration_count");

  await page.getByRole("button", { name: "添加筛选" }).click();
  await page.getByRole("button", { name: "展开标签名…候选" }).click();
  await page.getByRole("option", { name: "http.request.method" }).click();
  await page.getByRole("button", { name: "展开标签值…候选" }).click();
  await page.getByRole("option", { name: "GET" }).click();
  await expect(editor).toContainText(
    'http.server.request.duration_count{http.request.method="GET"}',
  );

  await page.getByRole("button", { name: "执行查询" }).click();
  await expect(page.getByText(/加载耗时：\d+ ms\u3000结果序列：\d+/)).toBeVisible();

  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("rate(a[5m]) / rate(b[5m])");
  await expect(page.getByText("复杂表达式请使用代码模式编辑")).toBeVisible();
  await expect(editor).toContainText("rate(a[5m]) / rate(b[5m])");

  await page.getByRole("button", { name: "切换到代码模式" }).click();
  await expect(editor).toContainText("rate(a[5m]) / rate(b[5m])");
});

test("日志：执行 LogsQL、展示结果并打开详情", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table");

  await expect(page.getByLabel("LogsQL 表达式")).toContainText("*");
  const detail = page.getByRole("button", { name: "查看日志详情" }).first();
  await expect(detail).toBeVisible();
  await detail.click();
  await expect(page.getByRole("tab", { name: "字段" })).toBeVisible();
  await page.getByRole("tab", { name: "原始 JSON" }).click();
  await expect(page.getByRole("tab", { name: "原始 JSON" })).toHaveAttribute(
    "data-state",
    "active",
  );
});

test("日志工作区：draft、loading 与重置遵循执行快照", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table");
  const detail = page.getByRole("button", { name: "查看日志详情" }).first();
  await expect(detail).toBeVisible();

  const editor = page.getByLabel("LogsQL 表达式");
  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("error");
  await expect(page).toHaveURL(/q=error/);
  await expect(page).not.toHaveURL(/[?&]run=/);
  await expect(detail).toBeVisible();

  await page.getByRole("button", { name: "查询", exact: true }).click();
  await expect(page.getByRole("status").filter({ hasText: "正在拉取日志" })).toBeVisible();
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  await page.getByRole("button", { name: "重置", exact: true }).click();
  await expect(page).not.toHaveURL(/[?&]q=/);
  await expect(page).not.toHaveURL(/[?&]run=/);
  await expect(page.getByText("开始查询日志")).toBeVisible();
});

test("日志工作区：查询内时间范围解析期间展示 loading", async ({ page }) => {
  await openFeature(page, "/logs?q=_time%3A5m&run=1&view=table");

  await expect(page.getByRole("status").filter({ hasText: "正在拉取日志" })).toBeVisible();
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();
});

test("日志工作区：Live 暂停会关闭流并可从干净窗口恢复", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=live");
  await expect(page.getByText("已连接", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: "暂停", exact: true }).click();
  await expect(page.getByText("已暂停", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "继续", exact: true }).click();
  await expect(page.getByText("已连接", { exact: true })).toBeVisible();
  await expect(page.getByText(/空档/)).toHaveCount(0);
});

test("日志工作区：取消、项目切换与模式切换保持单一执行身份", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table");
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  const editor = page.getByLabel("LogsQL 表达式");
  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("__empty");
  await page.getByRole("button", { name: "查询", exact: true }).click();
  await expect(page.getByRole("button", { name: "取消查询" })).toBeVisible();
  await page.getByRole("button", { name: "取消查询" }).click();
  await expect(page.getByRole("button", { name: "查询", exact: true })).toBeVisible();

  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("*");
  await page.getByRole("button", { name: "查询", exact: true }).click();
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  await page.getByRole("button", { name: "切换项目" }).click();
  await page.getByRole("menuitem").filter({ hasText: "预发布" }).click();
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  await page.getByRole("tab", { name: "实时" }).click();
  await expect(page).toHaveURL(/view=live/);
  await expect(page.getByText("已连接", { exact: true })).toBeVisible();
  await page.getByRole("tab", { name: "表格" }).click();
  await expect(page).not.toHaveURL(/view=live/);
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();
});

test("日志工作区：Live 有界行集在滚动离底后仍继续接收", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=live&rows_per_page=10");
  await expect(page.getByText("已连接", { exact: true })).toBeVisible();
  const viewport = page.getByLabel("实时日志结果");
  await expect(viewport.getByRole("button")).toHaveCount(10, { timeout: 8_000 });
  await expect(page.getByText(/已丢弃 [1-9]\d* 行旧数据/)).toBeVisible({ timeout: 8_000 });

  const droppedBefore = await page.getByText(/已丢弃 \d+ 行旧数据/).textContent();
  await viewport.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  await expect.poll(async () => page.getByText(/已丢弃 \d+ 行旧数据/).textContent())
    .not.toBe(droppedBefore);
  await expect(page.getByText("已连接", { exact: true })).toBeVisible();
});

test("日志工作区：Live 查询错误终止并提供显式重试", async ({ page }) => {
  await openFeature(page, "/logs?q=__error&run=1&view=live");
  await expect(page.getByText(/LogsQL parse error near: __error/)).toBeVisible();
  await page.getByRole("button", { name: "重试", exact: true }).click();
  await expect(page.getByText("正在连接", { exact: true })).toBeVisible();
  await expect(page.getByText(/LogsQL parse error near: __error/)).toBeVisible();
  await expect(page.getByText("正在重连…", { exact: true })).toHaveCount(0);
});

test("日志工作区：辅助查询失败不遮蔽主结果", async ({ page }) => {
  await openFeature(page, "/logs?q=__chart_error&run=1&view=table");
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();
  await expect(page.getByText("暂时无法加载数据，请重试。")).toBeVisible();
  await expect(page.getByText(/LogsQL chart error near: __chart_error/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "重试", exact: true })).toBeVisible();
});

test("日志工作区：外部导航和自动刷新不会形成 URL 循环", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table&refresh=5");
  const firstRow = page.getByRole("button", { name: "查看日志详情" }).first();
  await expect(firstRow).toBeVisible();
  const refreshingURL = page.url();
  await page.waitForTimeout(5_200);
  await expect(firstRow).toBeVisible();
  expect(page.url()).toBe(refreshingURL);

  await page.goto("/logs?q=error&run=0&view=table");
  await expect(page.getByLabel("LogsQL 表达式")).toContainText("error");
  await expect(page.getByText("开始查询日志")).toBeVisible();
  await page.goBack();
  await expect(page).not.toHaveURL(/[?&]q=/);
  await expect(page).toHaveURL(/run=1/);
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();
});

test("日志工作区：自动刷新 loading 时跳过重叠 tick", async ({ page }) => {
  await openFeature(page, "/logs?q=__slow&run=1&view=table&refresh=1&hide_chart=1");
  const detail = page.getByRole("button", { name: "查看日志详情" }).first();
  await expect(detail).toBeVisible({ timeout: 4_000 });
  await expect(page.getByRole("status").filter({ hasText: "正在拉取日志" }))
    .toBeVisible({ timeout: 2_000 });
  await page.waitForTimeout(1_100);
  await expect(detail).toBeVisible({ timeout: 1_000 });
});

test("日志工作区：下载使用 executed snapshot 而不是未运行 draft", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table&hide_chart=1");
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  const editor = page.getByLabel("LogsQL 表达式");
  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("__empty");
  await expect(page).not.toHaveURL(/[?&]run=/);

  await page.evaluate(() => {
    const downloadTarget = window as unknown as {
      capturedDownload: number[];
      showSaveFilePicker: () => Promise<{
        createWritable: () => Promise<WritableStream<Uint8Array>>;
      }>;
    };
    downloadTarget.capturedDownload = [];
    downloadTarget.showSaveFilePicker = async () => ({
      createWritable: async () => new WritableStream<Uint8Array>({
        write(chunk) {
          downloadTarget.capturedDownload.push(...chunk);
        },
      }),
    });
  });
  await page.getByRole("button", { name: "下载", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "下载", exact: true }).click();
  await expect.poll(() => page.evaluate(() => {
    const bytes = (window as unknown as { capturedDownload: number[] }).capturedDownload;
    return new TextDecoder().decode(new Uint8Array(bytes)).trim();
  })).not.toBe("");
});

test("链路：检索 trace 并进入瀑布详情", async ({ page }) => {
  await openFeature(page, "/traces");

  const search = page.getByRole("button", { name: "检索" });
  await expect(search).toBeEnabled();
  await search.click();
  await expect(page.getByText(/\d+ 条链路/)).toBeVisible();

  await page.locator('main [role="button"][tabindex="0"]').first().click();
  await expect(page).toHaveURL(/\/traces\/[a-f0-9]{32}$/);
  await expect(page.locator("main")).toContainText(/Trace ID:[a-f0-9]{32}/);
  await expect(page.getByText("悬停查看时长与占比")).toBeVisible();

  // 验证瀑布图折叠/展开与点击选中 Span 详情抽屉联动正常
  const waterfallRows = page.locator('[data-virtualized] [role="button"][tabindex="0"]');
  const initialCount = await waterfallRows.count();
  expect(initialCount).toBeGreaterThan(1);

  const rootToggle = page.locator('[data-virtualized] button[aria-expanded]').first();
  await expect(rootToggle).toHaveAttribute("aria-expanded", "true");
  await rootToggle.click();
  await expect(rootToggle).toHaveAttribute("aria-expanded", "false");
  expect(await waterfallRows.count()).toBeLessThan(initialCount);

  await rootToggle.click();
  await expect(rootToggle).toHaveAttribute("aria-expanded", "true");
  await expect(waterfallRows).toHaveCount(initialCount);

  await waterfallRows.first().click();
  await expect(page.getByRole("button", { name: "查看 Trace 精确日志" })).toBeVisible();
});

test("日志 JSON 视图：支持虚拟滚动、单行展开/收起与复制", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=json&limit=200");

  const list = page.locator('section[aria-label="JSON Lines"] [role="list"]');
  await expect(list).toBeVisible();
  await expect(list).toHaveAttribute("data-virtualized", "true");

  // 200 行仅挂载可见窗口 + overscan，远少于总行数
  const initialMounted = await list.getByRole("listitem").count();
  expect(initialMounted).toBeGreaterThan(0);
  expect(initialMounted).toBeLessThan(80);

  // 展开与收起第一条 JSON 日志（多行 pretty-print 切换）
  const firstExpand = list.getByRole("button", { name: "展开日志行" }).first();
  await firstExpand.click();
  const firstCollapse = list.getByRole("button", { name: "收起日志行" }).first();
  await expect(firstCollapse).toHaveAttribute("aria-expanded", "true");
  await expect(list.locator("pre").first()).toBeVisible();
  await firstCollapse.click();
  await expect(list.getByRole("button", { name: "展开日志行" }).first()).toHaveAttribute(
    "aria-expanded",
    "false",
  );

  // 滚动到底部后挂载后续日志行，DOM 节点仍维持有界窗口
  await list.evaluate((el) => {
    el.scrollTop = el.scrollHeight;
    el.dispatchEvent(new Event("scroll"));
  });
  await expect.poll(async () => {
    const indices = await list
      .getByRole("listitem")
      .evaluateAll((els) => els.map((el) => Number(el.getAttribute("data-virtual-index"))));
    return Math.max(...indices);
  }).toBeGreaterThanOrEqual(150);
  const scrolledMounted = await list.getByRole("listitem").count();
  expect(scrolledMounted).toBeLessThan(80);

  // 清空结果（卸载滚动容器）后重新拉取大结果集，滚动位置应复位到顶部且不出现白屏
  const editor = page.getByLabel("LogsQL 表达式");
  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("__empty");
  await page.getByRole("button", { name: "查询", exact: true }).click();
  await expect(page.getByText("无匹配日志")).toBeVisible();

  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("*");
  await page.getByRole("button", { name: "查询", exact: true }).click();
  await expect(list).toBeVisible();
  await expect(list.getByRole("listitem").first()).toHaveAttribute("data-virtual-index", "0");
});

test("问题：打开 Issue 并检查堆栈与关联入口", async ({ page }) => {
  await openFeature(page, "/issues");

  const issue = page.locator("main ul li button").first();
  await expect(issue).toBeVisible();
  await issue.click();
  await expect(page).toHaveURL(/\/issues\/[^/?]+/);
  await expect(page.getByText("堆栈跟踪", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "原始 JSON" })).toBeVisible();
});

test("回放：筛选错误会话并加载播放器与时间轴标记", async ({ page }) => {
  await openFeature(page, "/replays");

  await page.getByLabel("仅含错误").click();
  await expect(page.getByText(/\d+ 个会话/)).toBeVisible();
  await page.locator('main [role="button"][tabindex="0"]').first().click();
  await expect(page).toHaveURL(/\/replays\/[^/?]+/);
  await expect(page.locator("main")).toContainText(/会话 ID:sess_/);
  await expect(page.getByText("时间轴标记", { exact: true })).toBeVisible();
});

test("性能：展示 Web Vitals 并切换后端端点统计", async ({ page }) => {
  await openFeature(page, "/performance");

  await expect(page.getByRole("button", { name: /最大内容绘制/ })).toBeVisible();
  await expect(page.getByText("按页面聚合", { exact: true })).toBeVisible();

  await page.getByRole("tab", { name: "后端接口" }).click();
  await expect(page.getByRole("columnheader", { name: "P95" })).toBeVisible();
  await expect(page.getByText("req/s").first()).toBeVisible();
});

test("持续剖析：渲染火焰图、搜索函数并打开剖析记录", async ({ page }) => {
  await openFeature(page, "/profiling");

  await expect(page.getByText(/样本合计 \d+/)).toBeVisible();
  await page.getByPlaceholder("搜索函数名…").fill("collectFlameSamples");
  await expect(page.getByText("1 处匹配")).toBeVisible();

  await page.getByRole("button", { name: "剖析记录" }).click();
  await expect(page.getByText("当前时间窗口内采集的原始剖析文件，可下载 pprof。")).toBeVisible();
  await expect(page.getByRole("link", { name: "下载" }).first()).toBeVisible();
});

test("日志：刷新控件显示间隔并可切换", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table");

  const refresh = page.getByRole("button", { name: "立即刷新" });
  await expect(refresh).toContainText("刷新");

  const interval = page.getByRole("button", { name: "自动刷新" });
  await expect(interval).toContainText("关");

  await interval.click();
  await expect(page.getByRole("menuitem")).toHaveText([
    "关",
    "15s",
    "30s",
    "1m",
    "5m",
    "15m",
    "30m",
  ]);
  await page.getByRole("menuitem", { name: "15s" }).click();
  await expect(interval).toContainText("15s");
  await expect(page).toHaveURL(/[?&]refresh=15/);
});

test("告警规则：表达式按信号使用 PromQL / LogsQL 代码编辑器", async ({ page }) => {
  await openFeature(page, "/alerts/rules");
  await page.getByRole("button", { name: "新建规则" }).click();
  const dialog = page.getByRole("dialog");
  const promql = dialog.getByLabel("PromQL 表达式");
  await expect(promql).toBeVisible();
  await expect(dialog.locator(".cm-editor")).toHaveCount(1);
  await promql.click();
  await promql.pressSequentially("up == 0");
  await expect(promql).toContainText("up == 0");

  await dialog.getByRole("combobox").filter({ hasText: "指标" }).click();
  await page.getByRole("option", { name: "日志" }).click();
  await expect(dialog.getByLabel("LogsQL 表达式")).toBeVisible();
  await expect(dialog.locator(".cm-editor")).toHaveCount(1);
});
