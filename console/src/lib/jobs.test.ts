/// <reference types="node" />
import assert from "node:assert/strict";
import { test } from "node:test";
import type { Job } from "../api/types.ts";
import { age, ago, bytes, span } from "./format.ts";
import { applyUpdate, canCancel, jobTitle, progress, queueStats, sortJobs } from "./jobs.ts";

const job = (id: string, state: Job["state"], created: string, extra: Partial<Job> = {}): Job => ({ id, model: "m", kind: "chat", state, created, ...extra });

test("sortJobs: running, queued oldest first, then the rest newest first", () => {
  const jobs = [
    job("d1", "done", "2026-10-10T10:00:00Z"),
    job("q2", "queued", "2026-10-10T10:05:00Z"),
    job("r", "running", "2026-10-10T09:00:00Z"),
    job("q1", "queued", "2026-10-10T10:01:00Z"),
    job("d2", "failed", "2026-10-10T11:00:00Z"),
  ];
  assert.deepEqual(sortJobs(jobs).map((j) => j.id), ["r", "q1", "q2", "d2", "d1"]);
});

test("applyUpdate: a whole job, an event, a deletion", () => {
  let jobs: Record<string, Job> = {};
  jobs = applyUpdate(jobs, { jobId: "a", job: job("a", "queued", "2026-10-10T10:00:00Z") });
  assert.equal(jobs.a.state, "queued");
  jobs = applyUpdate(jobs, { jobId: "a", event: { type: "progress", step: 3, total: 10 } });
  assert.equal(progress(jobs.a), 0.3);
  jobs = applyUpdate(jobs, { jobId: "a", event: { type: "status", text: "loading" } });
  assert.equal(jobs.a.status, "loading");
  const same = applyUpdate(jobs, { jobId: "zzz", event: { type: "status", text: "x" } });
  assert.equal(same, jobs);
  jobs = applyUpdate(jobs, { jobId: "a" });
  assert.deepEqual(jobs, {});
});

test("canCancel, jobTitle, queueStats", () => {
  assert.ok(canCancel(job("a", "queued", "")));
  assert.ok(canCancel(job("a", "running", "")));
  assert.ok(!canCancel(job("a", "done", "")));
  assert.equal(jobTitle(job("a", "done", "", { request: { messages: [{ Role: "user", Content: "first" }, { Role: "assistant", Content: "x" }, { Role: "user", Content: " hi\n there " }] } })), "hi there");
  assert.equal(jobTitle(job("a", "done", "")), "a");
  assert.equal(jobTitle(job("a", "done", "", { request: { prompt: "x".repeat(200) } })).length, 120);
  assert.deepEqual(queueStats([job("a", "running", ""), job("b", "queued", ""), job("c", "queued", ""), job("d", "failed", "")]), { running: 1, queued: 2, failed: 1, done: 0 });
});

test("span, age, ago, bytes", () => {
  assert.equal(span(5.7), "5 s");
  assert.equal(span(125), "2 min");
  assert.equal(span(3600 * 3 + 60 * 20), "3 h 20 min");
  assert.equal(span(7200), "2 h");
  assert.equal(span(86400 * 2.5), "2 d");
  const now = Date.parse("2026-10-10T12:00:00Z");
  assert.equal(age("2026-10-10T11:55:00Z", now), "5 min");
  assert.equal(age(undefined, now), "");
  assert.equal(ago("2026-10-10T11:59:50Z", now), "just now");
  assert.equal(ago("2026-10-10T11:30:00Z", now), "30 min ago");
  assert.equal(ago(undefined, now), "never");
  assert.equal(ago("0001-01-01T00:00:00Z", now), "never");
  assert.equal(age("0001-01-01T00:00:00Z", now), "");
  assert.equal(bytes(1.5e9), "1.5 GB");
  assert.equal(bytes(0), "");
});
