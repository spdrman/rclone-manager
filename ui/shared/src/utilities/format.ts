const UNITS = ["B", "KB", "MB", "GB", "TB", "PB"];

export function bytes(n: number, digits = 1): string {
  if (n === 0) return "0 B";
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), UNITS.length - 1);
  const v = n / 1024 ** i;
  return (i === 0 ? v.toFixed(0) : v.toFixed(digits)) + " " + UNITS[i];
}

export function rate(bytesPerSecond: number): string {
  return bytes(bytesPerSecond, 0) + "/s";
}

export function duration(seconds: number): string {
  if (seconds < 60) return Math.round(seconds) + "s";
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  if (m < 60) return m + "m " + String(s).padStart(2, "0") + "s";
  const h = Math.floor(m / 60);
  return h + "h " + String(m % 60).padStart(2, "0") + "m";
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

export function clock(iso: string): string {
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false
  });
}

export function stamp(iso: string): string {
  const d = new Date(iso);
  return (
    d.toLocaleDateString(undefined, { month: "short", day: "2-digit" }) + " " + clock(iso)
  );
}

export function percent(fraction: number): string {
  return (fraction * 100).toFixed(1) + "%";
}
