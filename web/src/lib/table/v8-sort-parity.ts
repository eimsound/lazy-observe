/**
 * TanStack Table v8 picked a column's default sort comparator and first sort
 * direction from the data, and v9 changed both heuristics. These helpers
 * reproduce the v8 choices so the v9 DataTable sorts exactly as before.
 */

export type V8AutoSortKind = "datetime" | "alphanumeric" | "text" | "basic";

const reSplitAlphaNumeric = /([0-9]+)/gm;

/**
 * Mirrors v8 `column.getAutoSortingFn()`. v8 sampled `flatRows.slice(10)`, i.e.
 * the rows from index 10 onward, so tables with at most 10 rows always used the
 * basic comparator. v9 samples the first 10 rows instead.
 */
export function v8AutoSortKind(values: readonly unknown[]): V8AutoSortKind {
  let isString = false;
  for (const value of values.slice(10)) {
    if (Object.prototype.toString.call(value) === "[object Date]") return "datetime";
    if (typeof value === "string") {
      isString = true;
      if (value.split(reSplitAlphaNumeric).length > 1) return "alphanumeric";
    }
  }
  return isString ? "text" : "basic";
}

/**
 * Mirrors v8 `column.getAutoSortDir()`: only the first row's value decides, and
 * anything but a string (including a missing value) sorts descending first. v9
 * skips leading nullish values instead.
 */
export function v8SortDescFirst(values: readonly unknown[]): boolean {
  return typeof values[0] !== "string";
}
