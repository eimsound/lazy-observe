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
  await expect(page.getByText("等待查询").first()).toBeVisible();
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
  await expect(page.getByText("等待查询").first()).toBeVisible();
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
  await expect(page.getByText("根操作", { exact: true })).toBeVisible();

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
  await expect(page.getByText("无数据").first()).toBeVisible();

  await editor.click();
  await editor.press("Control+A");
  await editor.pressSequentially("*");
  await page.getByRole("button", { name: "查询", exact: true }).click();
  await expect(list).toBeVisible();
  await expect(list.getByRole("listitem").first()).toHaveAttribute("data-virtual-index", "0");
});

test("日志 JSON 视图：展开高行后滚回行顶收起不出现视口白屏", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=json&limit=500");

  const list = page.locator('section[aria-label="JSON Lines"] [role="list"]');
  await expect(list).toBeVisible();
  await expect(list).toHaveAttribute("data-virtualized", "true");

  // 滚到中间第 150 行附近并展开该行
  const targetIndex = 150;
  await list.evaluate((el, idx) => {
    el.scrollTop = idx * 24;
    el.dispatchEvent(new Event("scroll"));
  }, targetIndex);

  const targetRow = list.locator(`[role="listitem"][data-virtual-index="${targetIndex}"]`);
  await expect(targetRow).toBeVisible();
  await targetRow.getByRole("button", { name: "展开日志行" }).click();
  await expect(targetRow.getByRole("button", { name: "收起日志行" })).toHaveAttribute(
    "aria-expanded",
    "true",
  );

  // 模拟超大 JSON 载荷（~2,246px 高）触发 ResizeObserver 更新行高
  await targetRow.locator("pre").evaluate((pre) => {
    pre.style.minHeight = "2246px";
  });
  await expect
    .poll(() => targetRow.evaluate((el) => el.getBoundingClientRect().height))
    .toBeGreaterThan(2_200);

  // 在高行内部向下滚动至行底，再滚回该行顶部
  const rowTop = await list.evaluate((container, idx) => {
    const row = container.querySelector(`[role="listitem"][data-virtual-index="${idx}"]`);
    if (!row) return -1;
    const top =
      row.getBoundingClientRect().top -
      container.getBoundingClientRect().top +
      container.scrollTop;
    container.scrollTop = top + 1_600;
    container.dispatchEvent(new Event("scroll"));
    return top;
  }, targetIndex);
  expect(rowTop).toBeGreaterThan(0);

  await page.waitForTimeout(80);

  await list.evaluate((container, top) => {
    container.scrollTop = top;
    container.dispatchEvent(new Event("scroll"));
  }, rowTop);
  await page.waitForTimeout(80);

  // 点击收起高行：视口内必须保留可见日志行且当前行仍在视口内（绝不白屏）
  await targetRow.getByRole("button", { name: "收起日志行" }).click();
  await expect(targetRow.getByRole("button", { name: "展开日志行" })).toHaveAttribute(
    "aria-expanded",
    "false",
  );

  await expect
    .poll(() =>
      list.evaluate((container) => {
        const cRect = container.getBoundingClientRect();
        const items = Array.from(container.querySelectorAll('[role="listitem"]'));
        return items.filter((el) => {
          const r = el.getBoundingClientRect();
          return r.bottom > cRect.top && r.top < cRect.bottom;
        }).length;
      }),
    )
    .toBeGreaterThan(0);

  const targetInViewport = await list.evaluate((container, idx) => {
    const cRect = container.getBoundingClientRect();
    const row = container.querySelector(`[role="listitem"][data-virtual-index="${idx}"]`);
    if (!row) return false;
    const r = row.getBoundingClientRect();
    return r.bottom > cRect.top && r.top < cRect.bottom;
  }, targetIndex);
  expect(targetInViewport).toBe(true);
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
  await expect(page.getByRole("dialog").getByRole("heading", { name: "剖析记录" })).toBeVisible();
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

test("告警规则：评估间隔留空用信号默认值，越界值本地拦截", async ({ page }) => {
  await openFeature(page, "/alerts/rules");
  await expect(page.getByRole("columnheader", { name: "间隔" })).toBeVisible();
  await page.getByRole("button", { name: "新建规则" }).click();
  const dialog = page.getByRole("dialog");
  const interval = dialog.getByLabel("评估间隔");
  await expect(interval).toHaveAttribute("placeholder", "默认 5s");

  await dialog.getByRole("combobox").filter({ hasText: "指标" }).click();
  await page.getByRole("option", { name: "日志" }).click();
  await expect(interval).toHaveAttribute("placeholder", "默认 1m");

  await dialog.getByLabel("规则名称").fill("ErrorLogs");
  const logsql = dialog.getByLabel("LogsQL 表达式");
  await logsql.click();
  await logsql.pressSequentially("error | stats count() as n | filter n:>0");
  await interval.fill("10s");
  await dialog.getByRole("button", { name: "创建" }).click();
  await expect(dialog.getByRole("alert")).toHaveText("评估间隔须在 30s–1h 之间，且为 5s 的整数倍");
  await interval.fill("60");
  await dialog.getByRole("button", { name: "创建" }).click();
  await expect(dialog.getByRole("alert")).toHaveText("评估间隔格式无效，例如 30s、1m");
});

test("日志筛选：双栏弹层支持字段搜索、排序、包含/排除、任意非空与清空", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table&hide_chart=1");
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  await expect(page.getByRole("button", { name: "流字段" })).toHaveCount(0);
  await expect(page.locator('aside[aria-label="流字段"]')).toHaveCount(0);

  const trigger = page.getByRole("button", { name: /^筛选/ });
  await expect(trigger).toBeVisible();
  await trigger.click();

  const popover = page.getByRole("dialog", { name: "筛选" });
  await expect(popover).toBeVisible();
  await expect(popover.getByRole("option", { name: /^app\b/ })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(popover.getByRole("button", { name: "包含 api-gateway" })).toBeVisible();

  await popover.getByLabel("搜索字段").fill("env");
  await expect(popover.getByRole("option", { name: /^env\b/ })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  const envValues = popover.getByRole("list", { name: "字段 env 的可选值" });
  await expect(envValues.getByRole("listitem").first()).toContainText("prod");

  await popover.getByRole("button", { name: "按命中数升序" }).click();
  await expect(envValues.getByRole("listitem").first()).toContainText("staging");
  await popover.getByRole("option", { name: /^env\b/ }).click();
  await popover.getByRole("button", { name: "清除字段搜索" }).click();
  await expect(popover.getByRole("option", { name: /^env\b/ })).toHaveAttribute(
    "aria-selected",
    "true",
  );

  const activeFilters = page.getByLabel("当前附加过滤条件");
  const anyButton = popover.getByRole("button", { name: "任意", exact: true });
  await anyButton.click();
  await expect(anyButton).toHaveAttribute("aria-pressed", "true");
  await expect(activeFilters).toContainText('{env!=""}');
  await expect(trigger).toContainText("1");

  await popover.getByRole("option", { name: /^app\b/ }).click();
  await popover.getByRole("button", { name: "包含 api-gateway" }).click();
  await popover.getByRole("button", { name: "排除 worker" }).click();
  await expect(popover.getByRole("button", { name: "包含 api-gateway" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  await expect(popover.getByRole("button", { name: "排除 worker" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  await expect(activeFilters).toContainText('{app="api-gateway"}');
  await expect(activeFilters).toContainText('{app!="worker"}');
  await expect(trigger).toContainText("3");

  await popover.getByRole("option", { name: /^env\b/ }).click();
  await popover.getByRole("button", { name: "清除字段 env 的过滤条件" }).click();
  await expect(activeFilters).not.toContainText('{env!=""}');
  await expect(trigger).toContainText("2");

  await popover.getByRole("button", { name: "清空过滤条件" }).click();
  await expect(activeFilters).toHaveCount(0);
  await expect(trigger).toHaveText("筛选");

  await page.keyboard.press("Escape");
  await expect(popover).toHaveCount(0);

  await trigger.click();
  await expect(popover).toBeVisible();
  await page.locator("main").click({ position: { x: 10, y: 10 } });
  await expect(popover).toHaveCount(0);
});

test("日志筛选：移动端 390px 视口双栏弹层无横向溢出", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openFeature(page, "/logs?q=*&run=1&view=table&hide_chart=1");
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  const trigger = page.getByRole("button", { name: /^筛选/ });
  await trigger.click();

  const popover = page.getByRole("dialog", { name: "筛选" });
  await expect(popover).toBeVisible();
  await expect(popover.getByRole("option", { name: /^app\b/ })).toBeVisible();
  await expect(popover.getByRole("button", { name: "包含 api-gateway" })).toBeVisible();

  const layout = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    innerWidth: window.innerWidth,
  }));
  expect(layout.scrollWidth).toBeLessThanOrEqual(layout.innerWidth);

  const box = await popover.boundingBox();
  expect(box).not.toBeNull();
  expect(box!.x).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width).toBeLessThanOrEqual(390);

  await popover.getByRole("button", { name: "关闭筛选" }).click();
  await expect(popover).toHaveCount(0);
});

test("日志筛选：字段列表支持独立无障碍名称与键盘导航", async ({ page }) => {
  await openFeature(page, "/logs?q=*&run=1&view=table&hide_chart=1");
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  await page.getByRole("button", { name: /^筛选/ }).click();
  const popover = page.getByRole("dialog", { name: "筛选" });
  await expect(popover).toBeVisible();

  const fieldListbox = popover.getByRole("listbox", { name: "筛选字段" });
  await expect(fieldListbox).toBeVisible();

  const appOption = fieldListbox.getByRole("option", { name: /^app\b/ });
  const envOption = fieldListbox.getByRole("option", { name: /^env\b/ });
  const podOption = fieldListbox.getByRole("option", { name: /^pod\b/ });

  const searchInput = popover.getByLabel("搜索字段");
  await searchInput.focus();
  await searchInput.press("ArrowDown");
  await expect(appOption).toBeFocused();
  await expect(appOption).toHaveAttribute("aria-selected", "true");
  await expect(popover.getByRole("list", { name: "字段 app 的可选值" })).toBeVisible();

  await appOption.press("ArrowDown");
  await expect(envOption).toBeFocused();
  await expect(envOption).toHaveAttribute("aria-selected", "true");
  await expect(popover.getByRole("list", { name: "字段 env 的可选值" })).toBeVisible();

  await envOption.press("End");
  await expect(podOption).toBeFocused();
  await expect(podOption).toHaveAttribute("aria-selected", "true");
  await expect(popover.getByRole("list", { name: "字段 pod 的可选值" })).toBeVisible();

  await podOption.press("ArrowDown");
  await expect(podOption).toBeFocused();
  await expect(podOption).toHaveAttribute("aria-selected", "true");

  await podOption.press("ArrowUp");
  await expect(envOption).toBeFocused();
  await expect(envOption).toHaveAttribute("aria-selected", "true");

  await envOption.press("Home");
  await expect(appOption).toBeFocused();
  await expect(appOption).toHaveAttribute("aria-selected", "true");

  await appOption.press("ArrowUp");
  await expect(appOption).toBeFocused();
  await expect(appOption).toHaveAttribute("aria-selected", "true");

  await appOption.press("ArrowDown");
  await expect(envOption).toHaveAttribute("aria-selected", "true");
  await searchInput.focus();
  await searchInput.press("ArrowDown");
  await expect(envOption).toBeFocused();

  await popover.getByRole("button", { name: "包含 prod" }).click();
  await expect(page.getByLabel("当前附加过滤条件")).toContainText('{env="prod"}');
});

test("日志筛选：字段列表与字段值加载失败后支持点击重试恢复", async ({ page }) => {
  await openFeature(
    page,
    "/logs?q=__stream_fields_error_once%20__field_values_error_once&run=1&view=table&hide_chart=1",
  );
  await expect(page.getByRole("button", { name: "查看日志详情" }).first()).toBeVisible();

  await page.getByRole("button", { name: /^筛选/ }).click();
  const popover = page.getByRole("dialog", { name: "筛选" });
  await expect(popover).toBeVisible();

  const fieldsAlert = popover.getByRole("alert").filter({ hasText: "筛选字段加载失败" });
  await expect(fieldsAlert).toBeVisible();
  await fieldsAlert.getByRole("button", { name: "重试" }).click();

  await expect(fieldsAlert).toHaveCount(0);
  const fieldListbox = popover.getByRole("listbox", { name: "筛选字段" });
  await expect(fieldListbox.getByRole("option", { name: /^app\b/ })).toHaveAttribute(
    "aria-selected",
    "true",
  );

  const valuesAlert = popover.getByRole("alert").filter({ hasText: "字段值加载失败" });
  await expect(valuesAlert).toBeVisible();
  await valuesAlert.getByRole("button", { name: "重试" }).click();

  await expect(valuesAlert).toHaveCount(0);
  const includeApiGateway = popover.getByRole("button", { name: "包含 api-gateway" });
  await expect(includeApiGateway).toBeVisible();
  await includeApiGateway.click();
  await expect(page.getByLabel("当前附加过滤条件")).toContainText('{app="api-gateway"}');
});
