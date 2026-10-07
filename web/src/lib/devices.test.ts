// pnpm test: node --test (Node strips the types; no test framework needed).
import assert from "node:assert/strict";
import { test } from "node:test";
import type { Device } from "../api/types";
import { countdown, groupByUser, INVITE_TTL, initials, inviteDeadline, invitedByText } from "./devices.ts";

const dev = (id: string, user: string, extra: Partial<Device> = {}): Device => ({
  id,
  name: `Device ${id}`,
  user,
  created: "2026-10-01T10:00:00Z",
  lastSeen: "2026-10-01T10:00:00Z",
  ...extra,
});

test("groupByUser: this device's user first, names in order, no name last; one group per name whatever its case", () => {
  const groups = groupByUser([
    dev("a", "Zoe"),
    dev("b", ""),
    dev("c", "Philipp", { lastSeen: "2026-10-01T09:00:00Z" }),
    dev("d", "anna"),
    dev("e", "Philipp", { current: true }),
    dev("f", "Anna", { lastSeen: "2026-10-02T10:00:00Z" }),
    dev("g", "Philipp", { lastSeen: "2026-10-03T09:00:00Z" }),
  ]);
  assert.deepEqual(
    groups.map((g) => [g.user, g.devices.map((d) => d.id)]),
    [
      ["Philipp", ["e", "g", "c"]],
      ["anna", ["f", "d"]],
      ["Zoe", ["a"]],
      ["", ["b"]],
    ],
  );
});

test("groupByUser: a device without a user field is in the group without a name", () => {
  const d = dev("x", "");
  delete d.user;
  assert.deepEqual(groupByUser([d]), [{ user: "", devices: [d] }]);
});

test("invitedByText names the inviter, this device, or one that is gone; nothing for owners", () => {
  const owner = dev("o", "P", { owner: true, name: "Chrome on macOS" });
  const me = dev("m", "P", { owner: true, current: true });
  const all = [owner, me];
  assert.equal(invitedByText(owner, all), "");
  assert.equal(invitedByText(dev("i", "A", { invitedBy: "o" }), all), "invited by Chrome on macOS");
  assert.equal(invitedByText(dev("i", "A", { invitedBy: "m" }), all), "invited by this device");
  assert.equal(invitedByText(dev("i", "A", { invitedBy: "gone" }), all), "invited by a revoked device");
});

test("inviteDeadline trusts expires unless the clocks disagree", () => {
  const now = Date.parse("2026-10-04T10:00:00Z");
  assert.equal(inviteDeadline("2026-10-04T10:10:00Z", now), Date.parse("2026-10-04T10:10:00Z"));
  assert.equal(inviteDeadline("2026-10-04T10:09:30Z", now), Date.parse("2026-10-04T10:09:30Z"));
  // the hub's clock is behind (already expired here) or far ahead: 10 minutes from now
  assert.equal(inviteDeadline("2026-10-04T09:55:00Z", now), now + INVITE_TTL);
  assert.equal(inviteDeadline("2026-10-04T12:00:00Z", now), now + INVITE_TTL);
  assert.equal(inviteDeadline("nonsense", now), now + INVITE_TTL);
});

test("countdown", () => {
  assert.equal(countdown(600_000), "10:00");
  assert.equal(countdown(545_000), "9:05");
  assert.equal(countdown(41_200), "0:42");
  assert.equal(countdown(0), "0:00");
  assert.equal(countdown(-5_000), "0:00");
});

test("initials", () => {
  assert.equal(initials("Philipp"), "P");
  assert.equal(initials("anna maria  schmidt"), "AM");
  assert.equal(initials("  "), "?");
  assert.equal(initials("Ölaf"), "Ö");
});
