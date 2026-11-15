import { describe, expect, it } from "vitest";
import { describeCron } from "./cron-describe";

describe("describeCron", () => {
  it.each([
    ["* * * * *", "Every minute"],
    ["*/15 * * * *", "Every 15 minutes"],
    ["0 * * * *", "Every hour, on the hour"],
    ["30 * * * *", "Every hour at 30 past"],
    ["5 */2 * * *", "Every 2 hours, at 05 past"],
    ["0 3 * * *", "Every day at 03:00"],
    ["30 9,17 * * *", "Every day at 09:00 and 17:30".replace("09:00", "09:30")],
    ["0 9 * * 1-5", "Every weekday at 09:00"],
    ["0 9 * * mon-fri", "Every weekday at 09:00"],
    ["0 10 * * 0,6", "Every Saturday and Sunday at 10:00"],
    ["0 6 * * 1", "Every Monday at 06:00"],
    ["0 6 * * 1,5", "Every Monday and Friday at 06:00"],
    ["0 6 * * 0,1", "Every Monday and Sunday at 06:00"],
    ["0 6 * * 7", "Every Sunday at 06:00"],
    ["15 2 1 * *", "On the 1st of every month at 02:15"],
    ["0 0 1,15 * *", "On the 1st and 15th of every month at 00:00"],
    ["0 0 1 1 *", "Every year on January 1 at 00:00"],
    ["0 0 1 1,7 *", "On the 1st of January and July at 00:00"],
    ["@daily", "Every day at 00:00"],
    ["@hourly", "Every hour, on the hour"],
    ["  0   3 * * *  ", "Every day at 03:00"],
  ])("%s → %s", (expr, want) => {
    expect(describeCron(expr)).toBe(want);
  });

  it.each([
    "",
    "0 3 * *",
    "0 3 * * * *",
    "61 3 * * *",
    "0 25 * * *",
    "0 3 1 * 1",
    "0 3 L * *",
    "0 3 * * 1#2",
    "*/5 3 * * *",
    "0 3 5-1 * *",
  ])("falls back on %j", (expr) => {
    expect(describeCron(expr)).toBeNull();
  });
});
