/**
 * The number-to-sentence layer every surface renders through.
 *
 * It is a formatting file, so the interesting part is not the arithmetic
 * but the small set of decisions it fixes in one place: sizes are
 * base-1024, times are 24-hour and locale-formatted, and an age is always
 * an age rather than a timestamp the reader has to subtract. Those choices
 * being here rather than at each call site is what keeps two panels from
 * disagreeing about the same number, which matters more in this product
 * than in most: an operator comparing "last run" on two screens is trying
 * to decide whether their backups are current.
 */
const UNITS = ["B", "KB", "MB", "GB", "TB", "PB"];

/**
 * A byte count as an operator reads it.
 *
 * Base-1024 under base-10 labels, which is what NAS vendors, `df` and
 * every storage page in this product's own design canvas already show, so
 * matching them beats being pedantically correct and disagreeing with the
 * number the operator can see in their file manager.
 *
 * The byte scale drops the decimals regardless of `digits`, because a
 * fraction of a byte is not a quantity, and the zero case is spelled out
 * rather than computed: log(0) is negative infinity and would index off
 * the front of the unit table.
 */
export function bytes(n: number, digits = 1): string {
  if (n === 0) return "0 B";
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), UNITS.length - 1);
  const v = n / 1024 ** i;
  return (i === 0 ? v.toFixed(0) : v.toFixed(digits)) + " " + UNITS[i];
}

/** A transfer rate. No decimals at all: this number is read off a
 *  progress bar that updates every few seconds, and a moving tenth of a
 *  megabyte is noise an eye has to filter rather than information. */
export function rate(bytesPerSecond: number): string {
  return bytes(bytesPerSecond, 0) + "/s";
}

/** Freshness is the most safety-critical number in the product, so it is always
 *  rendered as an explicit age — never a bare date the operator has to subtract. */
export function relativeAge(iso: string | null, now = Date.now()): string {
  if (!iso) return "never";
  const ms = now - new Date(iso).getTime();
  const mins = Math.round(ms / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return mins + (mins === 1 ? " minute ago" : " minutes ago");
  const hours = Math.round(mins / 60);
  if (hours < 48) return hours + (hours === 1 ? " hour ago" : " hours ago");
  return Math.round(hours / 24) + " days ago";
}

/** Time of day, 24-hour, in the reader's own locale for everything except
 *  the hour convention. Backups run overnight and their logs are read the
 *  next morning, so an am/pm suffix on a list of consecutive events is one
 *  more thing to get wrong at a glance. */
export function clock(iso: string): string {
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false
  });
}

/** A date and time with no year. Everything this stamps is recent by
 *  construction (a run, an event, a verification), and where it is not,
 *  `relativeAge` is the function that says so honestly. */
export function stamp(iso: string): string {
  const d = new Date(iso);
  return (
    d.toLocaleDateString(undefined, { month: "short", day: "2-digit" }) + " " + clock(iso)
  );
}

/** A fraction as a percentage, one decimal. Takes 0..1 rather than
 *  0..100, so a caller dividing two byte counts hands the result straight
 *  over and never has to remember which side of the multiplication it is
 *  on. */
export function percent(fraction: number): string {
  return (fraction * 100).toFixed(1) + "%";
}
