import type { Device } from "../api/types";

/** One user's devices on the Devices page: "Philipp", or "" for devices from before user names. */
export interface UserGroup {
  user: string;
  devices: Device[];
}

/** Compares names the way people read them ("anna" next to "Anna", "Mac 2" before "Mac 10"). */
const byName = new Intl.Collator(undefined, { sensitivity: "base", numeric: true }).compare;

/**
 * The devices grouped by user: the group with this device first, then by name, the devices without a name last. In a
 * group: this device first, then the most recently seen.
 */
export function groupByUser(devices: Device[]): UserGroup[] {
  const groups = new Map<string, Device[]>();
  for (const d of devices) {
    const user = (d.user ?? "").trim();
    // one group per name, whatever its case ("anna" and "Anna"): the first spelling names it
    const key = [...groups.keys()].find((k) => byName(k, user) === 0) ?? user;
    groups.set(key, [...(groups.get(key) ?? []), d]);
  }
  const recent = (a: Device, b: Device) => Number(!!b.current) - Number(!!a.current) || b.lastSeen.localeCompare(a.lastSeen);
  const mine = (g: UserGroup) => g.devices.some((d) => d.current);
  return [...groups]
    .map(([user, list]) => ({ user, devices: list.sort(recent) }))
    .sort((a, b) => Number(mine(b)) - Number(mine(a)) || Number(!a.user) - Number(!b.user) || byName(a.user, b.user));
}

/** Who signed a device in: "invited by Safari on macOS", or the owner's password. */
export function invitedByText(d: Device, all: Device[]): string {
  if (d.owner || !d.invitedBy) return "";
  const by = all.find((x) => x.id === d.invitedBy);
  if (!by) return "invited by a revoked device";
  return `invited by ${by.current ? "this device" : by.name || "a device"}`;
}

/** An invite lives this long (the hub's inviteTTL). */
export const INVITE_TTL = 10 * 60 * 1000;

/**
 * When an invite runs out on this browser's clock: its `expires`, unless this clock and the hub's disagree so much
 * that it would end now or far later than possible — then 10 minutes from when it arrived.
 */
export function inviteDeadline(expires: string, received = Date.now()): number {
  const at = Date.parse(expires);
  const left = at - received;
  return Number.isFinite(at) && left > 5_000 && left <= INVITE_TTL + 60_000 ? at : received + INVITE_TTL;
}

/** The time left as the countdown shows it: "9:05", "0:42", "0:00". */
export function countdown(ms: number): string {
  const s = Math.max(0, Math.ceil(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

/** The two letters on a user's avatar: "P" for Philipp, "AM" for Anna Maria, "?" without a name. */
export function initials(user: string): string {
  const words = user.trim().split(/\s+/).filter(Boolean);
  if (!words.length) return "?";
  return words
    .slice(0, 2)
    .map((w) => [...w][0].toUpperCase())
    .join("");
}
