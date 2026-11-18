/** A 5-field cron expression in words, for the schedule lists. Covers the
 *  common shapes and returns the expression itself otherwise. Times read in
 *  `timezone` (the schedule's own zone; UTC when it has none). */
export function cronToHuman(expr: string, timezone = "UTC"): string {
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return expr;
  const [min, hour, dom, mon, dow] = parts;
  if (min === "*" && hour === "*") return `Every minute`;
  if (min.startsWith("*/")) return `Every ${min.slice(2)} minutes`;
  if (hour.startsWith("*/") && min === "0") return `Every ${hour.slice(2)} hours`;
  if (dom === "*" && mon === "*" && dow === "*" && !hour.startsWith("*/"))
    return `Daily at ${hour.padStart(2, "0")}:${min.padStart(2, "0")} ${timezone}`;
  if (dom === "*" && mon === "*" && dow === "1-5" && !hour.startsWith("*/"))
    return `Weekdays at ${hour.padStart(2, "0")}:${min.padStart(2, "0")} ${timezone}`;
  return expr;
}
