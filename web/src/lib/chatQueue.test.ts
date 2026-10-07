/// <reference types="node" />
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  claim,
  edit,
  emptyQueue,
  enqueue,
  move,
  moveTo,
  nextStep,
  pause,
  prune,
  queuedCount,
  remove,
  restore,
  resume,
  sendNow,
  sent,
  unsent,
  type ChatQueue,
  type TurnState,
} from "./chatQueue.ts";

const turn = (id: string, state: TurnState["state"], tools?: { state: string }[]): TurnState => ({ id, state, tools });
const texts = (q: ChatQueue) => q.items.map((i) => i.text);

/** a queue of a, b, c waiting for the running turn j1 */
function abc(): ChatQueue {
  let q = emptyQueue();
  q = enqueue(q, { id: "a", text: "first" }, ["j1"]);
  q = enqueue(q, { id: "b", text: "second" }, ["ignored"]);
  q = enqueue(q, { id: "c", text: "third" }, ["ignored"]);
  return q;
}

test("enqueue: in order, trimmed, empty ignored; waits for the turns running when the queue started", () => {
  let q = abc();
  assert.deepEqual(texts(q), ["first", "second", "third"]);
  assert.deepEqual(q.after, ["j1"]);
  q = enqueue(q, { id: "d", text: "   " }, []);
  assert.equal(q.items.length, 3);
  q = enqueue(q, { id: "e", text: "  x \n" }, [], { model: "m" });
  assert.equal(q.items.at(-1)!.text, "x");
  assert.deepEqual(q.config, { model: "m" });
  assert.equal(queuedCount(q), 4);
  assert.equal(queuedCount(undefined), 0);
});

test("edit, remove, move, moveTo", () => {
  let q = abc();
  q = edit(q, "b", " 2nd ");
  assert.deepEqual(texts(q), ["first", "2nd", "third"]);
  assert.deepEqual(texts(edit(q, "b", "  ")), ["first", "third"]);
  assert.deepEqual(texts(remove(q, "a")), ["2nd", "third"]);
  assert.deepEqual(texts(move(q, "c", -1)), ["first", "third", "2nd"]);
  assert.deepEqual(texts(move(q, "a", -1)), ["first", "2nd", "third"]); // clamped
  assert.deepEqual(texts(move(q, "c", 1)), ["first", "2nd", "third"]);
  assert.deepEqual(texts(moveTo(q, "a", 2)), ["2nd", "third", "first"]);
  assert.deepEqual(texts(moveTo(q, "c", 0)), ["third", "first", "2nd"]);
  assert.equal(moveTo(q, "nope", 0), q);
});

test("nextStep: waits while running, an approval pending, or the turns it sent are not known yet", () => {
  const q = abc();
  const done = turn("j1", "done");
  assert.deepEqual(nextStep(q, { active: true, pending: 0, leaves: [turn("j1", "running")], after: [turn("j1", "running")] }), { do: "wait" });
  assert.deepEqual(nextStep(q, { active: false, pending: 1, leaves: [done], after: [done] }), { do: "wait" });
  assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [done], after: [undefined] }), { do: "wait" });
  assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [done], after: [done] }), { do: "send", item: q.items[0] });
  assert.deepEqual(nextStep(emptyQueue(), { active: false, pending: 0, leaves: [done], after: [] }), { do: "idle" });
  assert.deepEqual(nextStep(pause(q, "failed"), { active: false, pending: 0, leaves: [done], after: [done] }), { do: "idle" });
});

test("nextStep: a failed, stopped or denied turn pauses; Continue lets it pass", () => {
  const q = abc();
  for (const [t, reason] of [
    [turn("j1", "failed"), "failed"],
    [turn("j1", "canceled"), "canceled"],
    [turn("j1", "done", [{ state: "done" }, { state: "denied" }]), "denied"],
  ] as const) {
    assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [t], after: [t] }), { do: "pause", reason });
    const r = resume(pause(q, reason), ["j1"]);
    assert.equal(r.paused, undefined);
    assert.deepEqual(nextStep(r, { active: false, pending: 0, leaves: [t], after: [t] }).do, "send");
  }
  // compare chat: one column failed is enough
  const ok = turn("j1", "done");
  const bad = turn("j2", "failed");
  assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [ok, bad], after: [ok] }), { do: "pause", reason: "failed" });
  // nothing to continue
  assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [], after: [ok] }), { do: "pause", reason: "gone" });
});

test("send: claim, sent → waits for the new turns, then the next", () => {
  let q = abc();
  const [item, claimed] = claim(q, 1000);
  assert.equal(item!.id, "a");
  assert.deepEqual(texts(claimed), ["second", "third"]);
  assert.equal(queuedCount(claimed), 3);
  assert.deepEqual(claim(claimed, 1001)[0], undefined); // one at a time
  assert.deepEqual(nextStep(claimed, { active: false, pending: 0, leaves: [turn("j1", "done")], after: [turn("j1", "done")] }), { do: "wait" });
  q = sent(claimed, ["j2", "j3"]);
  assert.equal(q.inflight, undefined);
  assert.deepEqual(q.after, ["j2", "j3"]);
  // the old leaf is still shown until the stream brings j2/j3
  assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [turn("j1", "done")], after: [undefined, undefined] }), { do: "wait" });
  const j2 = turn("j2", "done");
  const j3 = turn("j3", "done");
  assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [j2, j3], after: [j2, j3] }), { do: "send", item: q.items[0] });
});

test("unsent: back to the front, paused", () => {
  const [, claimed] = claim(abc(), 1);
  const q = unsent(claimed);
  assert.deepEqual(texts(q), ["first", "second", "third"]);
  assert.equal(q.paused, "error");
  assert.equal(q.inflight, undefined);
});

test("sendNow: goes first, the stopped turn does not pause the queue", () => {
  const q = sendNow(pause(abc(), "failed"), "c", ["j1"], ["j1"]);
  assert.deepEqual(texts(q), ["third", "first", "second"]);
  assert.equal(q.paused, undefined);
  const stopped = turn("j1", "canceled");
  assert.deepEqual(nextStep(q, { active: true, pending: 0, leaves: [turn("j1", "running")], after: [turn("j1", "running")] }), { do: "wait" });
  assert.deepEqual(nextStep(q, { active: false, pending: 0, leaves: [stopped], after: [stopped] }), { do: "send", item: q.items[0] });
});

test("enqueue into an emptied queue waits for the turns running now and forgets old acks", () => {
  let q = sendNow(abc(), "a", ["j1"], ["j1"]);
  q = remove(remove(remove(q, "a"), "b"), "c");
  q = enqueue(q, { id: "d", text: "again" }, ["j9"]);
  assert.deepEqual(q.after, ["j9"]);
  assert.deepEqual(q.ack, []);
});

test("restore: resumes only when the turn it waits for runs or ended well", () => {
  const q = abc();
  const at = (t?: TurnState) => () => t;
  assert.equal(restore(q, at(turn("j1", "running"))), q);
  assert.equal(restore(q, at(turn("j1", "queued"))), q);
  assert.equal(restore(q, at(turn("j1", "done"))), q);
  assert.equal(restore(q, at(turn("j1", "failed"))).paused, "failed");
  assert.equal(restore(q, at(turn("j1", "canceled"))).paused, "canceled");
  assert.equal(restore(q, at(turn("j1", "done", [{ state: "denied" }]))).paused, "denied");
  assert.equal(restore(q, at(undefined)).paused, "gone");
  assert.equal(restore({ ...q, after: [] }, at(turn("j1", "done"))).paused, "gone");
  // acknowledged (Send now / Continue): its end does not matter
  assert.equal(restore(sendNow(q, "a", ["j1"], []), at(turn("j1", "canceled"))).paused, undefined);
  // already paused or empty: as is
  const p = pause(q, "failed");
  assert.equal(restore(p, at(turn("j1", "done"))), p);
  const e = emptyQueue();
  assert.equal(restore(e, at(undefined)), e);
  // being sent when the page went away: back in front, paused
  const [, claimed] = claim(q, 5);
  const r = restore(claimed, at(turn("j1", "done")));
  assert.equal(r.paused, "interrupted");
  assert.deepEqual(texts(r), ["first", "second", "third"]);
});

test("prune: drops empty queues and those of deleted chats", () => {
  const all = { c1: abc(), c2: emptyQueue(), c3: abc() };
  assert.deepEqual(Object.keys(prune(all)), ["c1", "c3"]);
  assert.deepEqual(Object.keys(prune(all, (id) => id !== "c3")), ["c1"]);
});
