import { expect, test, type Locator, type Page } from "@playwright/test";

// Covers the shared DataTable through the metrics legend table: column layout and
// width, sorting cycles (numeric/string, multi-sort, reset), and row click selection.

const mockAdmin = JSON.stringify({ username: "admin", role: "admin" });

async function openLegend(page: Page, tableMode: "labels" | "series" = "labels"): Promise<Locator> {
  await page.addInitScript(
    ({ session }) => {
      window.localStorage.setItem("lo-language", "zh");
      window.localStorage.setItem("lo-mock-session", session);
    },
    { session: mockAdmin },
  );
  const params = new URLSearchParams({ "g0.expr": "http_requests_total", "g0.tab": "graph" });
  if (tableMode === "series") params.set("g0.table_mode", "series");
  await page.goto(`/metrics?${params.toString()}`);
  const table = page.locator("table").filter({
    has: page.getByRole("columnheader", { name: "平均值" }),
  });
  await expect(table).toBeVisible();
  await expect(table.locator("tbody tr").first()).toBeVisible();
  return table;
}

async function headerNames(table: Locator): Promise<string[]> {
  return (await table.locator("thead th").allInnerTexts()).map((text) => text.trim());
}

async function columnTexts(table: Locator, header: string): Promise<string[]> {
  const index = (await headerNames(table)).indexOf(header);
  expect(index, `column ${header}`).toBeGreaterThanOrEqual(0);
  return table.locator(`tbody tr td:nth-child(${index + 1})`).allInnerTexts();
}

async function columnNumbers(table: Locator, header: string): Promise<number[]> {
  return (await columnTexts(table, header)).map((text) => Number(text.replaceAll(",", "")));
}

function sortButton(table: Locator, header: string): Locator {
  return table.locator("thead th").filter({ hasText: header }).getByRole("button", { name: "排序此列" });
}

async function sortIcon(table: Locator, header: string): Promise<"asc" | "desc" | "none"> {
  const button = sortButton(table, header);
  if (await button.locator("svg.lucide-arrow-up").count()) return "asc";
  if (await button.locator("svg.lucide-arrow-down").count()) return "desc";
  await expect(button.locator("svg.lucide-chevrons-up-down")).toHaveCount(1);
  return "none";
}

async function selection(table: Locator): Promise<string[]> {
  return table.locator("tbody tr").evaluateAll((rows) => rows.map((row) => row.getAttribute("aria-selected") ?? ""));
}

const ascending = (values: number[]) => [...values].sort((a, b) => a - b);
const descending = (values: number[]) => [...values].sort((a, b) => b - a);
const STAT_HEADERS = ["平均值", "最大值", "最小值", "最新值", "合计"];

test("指标图例表：标签分列与完整指标两种列布局及表格最小宽度", async ({ page }) => {
  const table = await openLegend(page, "labels");
  expect(await headerNames(table)).toEqual(["指标名", "instance", "job", "series", ...STAT_HEADERS]);
  await expect(table.locator("thead th button[aria-label='排序此列']")).toHaveCount(9);
  // 220 + 3 * 160 + 5 * 96 = 1180 > minWidth 960
  await expect(table).toHaveCSS("min-width", "1180px");
  const rows = await table.locator("tbody tr").count();
  expect(rows).toBeGreaterThanOrEqual(3);
  expect(await columnTexts(table, "series")).toEqual(Array.from({ length: rows }, (_, i) => String(i + 1)));
  // Stat columns are right aligned and monospaced; label columns are left aligned.
  const avgIndex = (await headerNames(table)).indexOf("平均值");
  await expect(table.locator(`tbody tr:first-child td:nth-child(${avgIndex + 1})`)).toHaveClass(/text-right/);
  await expect(table.locator("tbody tr:first-child td:nth-child(2)")).toHaveClass(/text-left/);

  const seriesPage = await page.context().newPage();
  const seriesTable = await openLegend(seriesPage, "series");
  expect(await headerNames(seriesTable)).toEqual([`完整指标（${rows}）`, ...STAT_HEADERS]);
  // 480 + 5 * 96 = 960 == minWidth
  await expect(seriesTable).toHaveCSS("min-width", "960px");
  await expect(seriesTable.locator("tbody tr").first().locator("td").first()).toContainText('http_requests_total{instance="');
});

test("指标图例表：数值列排序按 降序→升序→原始顺序 循环", async ({ page }) => {
  const table = await openLegend(page);
  const originalSeries = await columnTexts(table, "series");
  // Mock values depend on query time and may be refreshed, so numeric order is checked
  // against the rendered column itself; series labels are stable and prove row identity.
  expect(await sortIcon(table, "最大值")).toBe("none");

  await sortButton(table, "最大值").click();
  expect(await sortIcon(table, "最大值")).toBe("desc");
  const desc = await columnNumbers(table, "最大值");
  expect(desc).toHaveLength(originalSeries.length);
  expect(desc).toEqual(descending(desc));

  await sortButton(table, "最大值").click();
  expect(await sortIcon(table, "最大值")).toBe("asc");
  const asc = await columnNumbers(table, "最大值");
  expect(asc).toEqual(ascending(asc));
  expect(asc).not.toEqual(desc);

  await sortButton(table, "最大值").click();
  expect(await sortIcon(table, "最大值")).toBe("none");
  expect(await columnTexts(table, "series")).toEqual(originalSeries);
});

test("指标图例表：字符串列排序按 升序→降序→原始顺序 循环，点击另一列替换排序", async ({ page }) => {
  const table = await openLegend(page);
  const originalSeries = await columnTexts(table, "series");
  const sortedSeries = [...originalSeries].sort();

  await sortButton(table, "series").click();
  expect(await sortIcon(table, "series")).toBe("asc");
  expect(await columnTexts(table, "series")).toEqual(sortedSeries);

  await sortButton(table, "series").click();
  expect(await sortIcon(table, "series")).toBe("desc");
  expect(await columnTexts(table, "series")).toEqual([...sortedSeries].reverse());

  // A plain click on another column replaces the current sort.
  await sortButton(table, "平均值").click();
  expect(await sortIcon(table, "series")).toBe("none");
  expect(await sortIcon(table, "平均值")).toBe("desc");
  const avg = await columnNumbers(table, "平均值");
  expect(avg).toEqual(descending(avg));

  await sortButton(table, "平均值").click();
  await sortButton(table, "平均值").click();
  expect(await sortIcon(table, "平均值")).toBe("none");
  expect(await columnTexts(table, "series")).toEqual(originalSeries);
});

test("指标图例表：Shift+点击追加次级排序", async ({ page }) => {
  const table = await openLegend(page);
  const jobs = await columnTexts(table, "job");
  const series = await columnTexts(table, "series");
  const original = jobs.map((job, i) => ({ job, series: series[i] }));
  // The mock query yields 5 series over 3 jobs, so the secondary key is observable.
  expect(new Set(jobs).size).toBeLessThan(jobs.length);

  await sortButton(table, "job").click();
  await sortButton(table, "series").click({ modifiers: ["Shift"] });
  await sortButton(table, "series").click({ modifiers: ["Shift"] });
  expect(await sortIcon(table, "job")).toBe("asc");
  expect(await sortIcon(table, "series")).toBe("desc");

  const expected = [...original].sort((a, b) =>
    a.job === b.job ? b.series.localeCompare(a.series) : a.job.localeCompare(b.job)
  );
  expect(await columnTexts(table, "job")).toEqual(expected.map((row) => row.job));
  expect(await columnTexts(table, "series")).toEqual(expected.map((row) => row.series));
  const jobOnly = [...original].sort((a, b) => a.job.localeCompare(b.job));
  expect(expected.map((row) => row.series)).not.toEqual(jobOnly.map((row) => row.series));
});

test("指标图例表：切换分列模式重置排序", async ({ page }) => {
  const table = await openLegend(page);
  const originalSeries = await columnTexts(table, "series");
  await sortButton(table, "series").click();
  await sortButton(table, "series").click();
  expect(await columnTexts(table, "series")).toEqual([...originalSeries].sort().reverse());

  await page.getByRole("button", { name: "图表设置" }).click();
  await page.getByRole("button", { name: "完整指标" }).click();
  await expect(table.locator("thead th").first()).toContainText("完整指标（");
  await page.getByRole("button", { name: "标签分列" }).click();
  await expect(table.locator("thead th").first()).toHaveText("指标名");
  await page.keyboard.press("Escape");

  expect(await sortIcon(table, "series")).toBe("none");
  expect(await columnTexts(table, "series")).toEqual(originalSeries);
});

test("指标图例表：行点击选择、Ctrl+点击隐藏、拖动不触发、排序后仍命中原序列", async ({ page }) => {
  const table = await openLegend(page);
  const rows = table.locator("tbody tr");
  const count = await rows.count();
  const all = (value: string) => Array.from({ length: count }, () => value);
  expect(await selection(table)).toEqual(all("true"));
  await expect(rows.first()).toHaveClass(/cursor-pointer/);

  // Single click isolates one series; clicking it again restores all.
  await rows.nth(1).click();
  expect(await selection(table)).toEqual(all("false").map((v, i) => (i === 1 ? "true" : v)));
  await expect(rows.nth(0)).toHaveCSS("opacity", "0.45");
  await expect(rows.nth(1)).toHaveCSS("opacity", "1");
  await rows.nth(1).click();
  expect(await selection(table)).toEqual(all("true"));

  // Ctrl+click hides one series.
  await rows.nth(0).click({ modifiers: ["Control"] });
  expect(await selection(table)).toEqual(all("true").map((v, i) => (i === 0 ? "false" : v)));
  await rows.nth(0).click({ modifiers: ["Control"] });
  expect(await selection(table)).toEqual(all("true"));

  // A drag (text selection) across a row must not toggle selection.
  const box = (await rows.nth(2).boundingBox())!;
  await page.mouse.move(box.x + 20, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + 120, box.y + box.height / 2, { steps: 5 });
  await page.mouse.up();
  expect(await selection(table)).toEqual(all("true"));

  // After sorting, clicking a row selects that row's series, not the original index.
  await sortButton(table, "series").click();
  await sortButton(table, "series").click();
  const sortedSeries = await columnTexts(table, "series");
  await rows.first().click();
  const selected = await selection(table);
  expect(selected[0]).toBe("true");
  expect(selected.filter((value) => value === "true")).toHaveLength(1);
  expect(sortedSeries[0]).toBe(String(count));
  // Clearing the sort shows the same series (originally last) still selected.
  await sortButton(table, "series").click();
  expect(await selection(table)).toEqual(all("false").map((v, i) => (i === count - 1 ? "true" : v)));
});
