/**
 * A 5-field cron expression in plain English, for the common shapes.
 *
 * Deliberately small: it reads what people actually write — every N minutes,
 * hourly, daily at a time, on weekdays, on a day of the month — and answers
 * `null` for anything else, so the caller falls back to the raw expression
 * rather than to a sentence that is nearly right. A schedule described wrongly
 * is worse than one not described at all.
 *
 * The zone is not part of the expression here: callers split it off with
 * `splitSchedule` and say it beside the sentence.
 */

const DAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const MONTH_NAMES = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];
const DAY_ABBR: Record<string, number> = { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 };
const MONTH_ABBR: Record<string, number> = {
  jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6, jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12,
};

const MACROS: Record<string, string> = {
  "@yearly": "0 0 1 1 *",
  "@annually": "0 0 1 1 *",
  "@monthly": "0 0 1 * *",
  "@weekly": "0 0 * * 0",
  "@daily": "0 0 * * *",
  "@midnight": "0 0 * * *",
  "@hourly": "0 * * * *",
};

/** A single whole number within bounds, or null. */
function num(field: string, lo: number, hi: number, names?: Record<string, number>): number | null {
  const lower = field.toLowerCase();
  if (names && lower in names) return names[lower];
  if (!/^\d+$/.test(field)) return null;
  const n = Number(field);
  return n >= lo && n <= hi ? n : null;
}

/** A list of numbers and ranges ("1-5", "1,3,5", "mon-fri"), expanded and sorted, or null. */
function list(field: string, lo: number, hi: number, names?: Record<string, number>): number[] | null {
  const out = new Set<number>();
  for (const part of field.split(",")) {
    const range = part.split("-");
    if (range.length === 1) {
      const n = num(range[0], lo, hi, names);
      if (n === null) return null;
      out.add(n);
    } else if (range.length === 2) {
      const a = num(range[0], lo, hi, names);
      const b = num(range[1], lo, hi, names);
      if (a === null || b === null || a > b) return null;
      for (let i = a; i <= b; i++) out.add(i);
    } else {
      return null;
    }
  }
  return [...out].sort((x, y) => x - y);
}

/** "*\/N" → N, or null. */
function step(field: string): number | null {
  const m = /^\*\/(\d+)$/.exec(field);
  if (!m) return null;
  const n = Number(m[1]);
  return n > 0 ? n : null;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

/** "a", "a and b", "a, b and c". */
function and(items: string[]): string {
  if (items.length <= 1) return items.join("");
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

function ordinalDay(n: number): string {
  const suffix = n % 100 >= 11 && n % 100 <= 13 ? "th"
    : n % 10 === 1 ? "st" : n % 10 === 2 ? "nd" : n % 10 === 3 ? "rd" : "th";
  return `${n}${suffix}`;
}

/** The days-of-week part: "every day", "every weekday", "every Monday and Friday". */
function weekdays(field: string): string | null {
  if (field === "*") return "every day";
  const days = list(field, 0, 7, DAY_ABBR);
  if (!days) return null;
  const set = [...new Set(days.map((d) => (d === 7 ? 0 : d)))].sort((a, b) => a - b);
  const key = set.join(",");
  if (key === "1,2,3,4,5") return "every weekday";
  if (key === "0,6") return "every Saturday and Sunday";
  if (key === "0,1,2,3,4,5,6") return "every day";
  // Sunday last reads the way a week is spoken about.
  const ordered = [...set.filter((d) => d !== 0), ...set.filter((d) => d === 0)];
  return `every ${and(ordered.map((d) => DAY_NAMES[d]))}`;
}

/**
 * The sentence, or null when the expression is not one of the shapes this
 * knows. Times are 24-hour, because that is how the expression states them.
 */
export function describeCron(expression: string): string | null {
  let expr = expression.trim().replace(/\s+/g, " ");
  if (expr === "") return null;
  expr = MACROS[expr.toLowerCase()] ?? expr;
  const fields = expr.split(" ");
  if (fields.length !== 5) return null;
  const [min, hour, dom, mon, dow] = fields;

  // Sub-hourly: every minute, or every N minutes, any day.
  if (hour === "*" && dom === "*" && mon === "*" && dow === "*") {
    if (min === "*") return "Every minute";
    const n = step(min);
    if (n !== null) return n === 1 ? "Every minute" : `Every ${n} minutes`;
    const m = num(min, 0, 59);
    if (m !== null) return m === 0 ? "Every hour, on the hour" : `Every hour at ${pad(m)} past`;
    return null;
  }

  const minute = num(min, 0, 59);
  if (minute === null) return null;

  // Every N hours.
  const hourStep = step(hour);
  if (hourStep !== null && dom === "*" && mon === "*") {
    const days = weekdays(dow);
    if (!days) return null;
    const every = hourStep === 1 ? "Every hour" : `Every ${hourStep} hours`;
    const at = minute === 0 ? "on the hour" : `at ${pad(minute)} past`;
    return days === "every day" ? `${every}, ${at}` : `${every}, ${at}, ${days}`;
  }

  // Every hour at a minute, on some days.
  if (hour === "*" && dom === "*" && mon === "*") {
    const days = weekdays(dow);
    if (!days) return null;
    const at = minute === 0 ? "on the hour" : `at ${pad(minute)} past`;
    return `Every hour, ${at}, ${days}`;
  }

  const hours = list(hour, 0, 23);
  if (!hours) return null;
  const times = and(hours.map((h) => `${pad(h)}:${pad(minute)}`));

  // Days of the week.
  if (dom === "*" && mon === "*") {
    const days = weekdays(dow);
    if (!days) return null;
    return `${days.charAt(0).toUpperCase()}${days.slice(1)} at ${times}`;
  }

  // A day of the month, every month or in named months.
  if (dow === "*") {
    const doms = list(dom, 1, 31);
    if (!doms) return null;
    const onDays = `the ${and(doms.map(ordinalDay))}`;
    if (mon === "*") return `On ${onDays} of every month at ${times}`;
    const months = list(mon, 1, 12, MONTH_ABBR);
    if (!months) return null;
    if (months.length === 1 && doms.length === 1) {
      return `Every year on ${MONTH_NAMES[months[0] - 1]} ${doms[0]} at ${times}`;
    }
    return `On ${onDays} of ${and(months.map((m) => MONTH_NAMES[m - 1]))} at ${times}`;
  }

  return null;
}
