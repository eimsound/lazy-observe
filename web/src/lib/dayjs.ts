import baseDayjs, { type ConfigType, type Dayjs } from "dayjs";
import customParseFormat from "dayjs/plugin/customParseFormat.js";
import localizedFormat from "dayjs/plugin/localizedFormat.js";
import relativeTime from "dayjs/plugin/relativeTime.js";
import timezone from "dayjs/plugin/timezone.js";
import utc from "dayjs/plugin/utc.js";
import "dayjs/locale/en.js";
import "dayjs/locale/zh-cn.js";

baseDayjs.extend(customParseFormat);
baseDayjs.extend(localizedFormat);
baseDayjs.extend(relativeTime);
baseDayjs.extend(utc);
baseDayjs.extend(timezone);

export type AppDayjsLocale = "en" | "zh-cn";

export function dayjsLocale(language?: string): AppDayjsLocale {
  return language?.toLowerCase().startsWith("zh") ? "zh-cn" : "en";
}

export function localizedDayjs(value: ConfigType, language?: string): Dayjs {
  return baseDayjs(value).locale(dayjsLocale(language));
}

export default baseDayjs;
