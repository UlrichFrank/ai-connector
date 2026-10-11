/// <reference types="node" />
import assert from "node:assert/strict";
import { test } from "node:test";
import type { Connector } from "../api/types.ts";
import { addAgent, addGenerator, agentId, agentKind, agentsProblem, createBody, emptyForm, formOf, moveAgent, moveGenerator, normalizeUrl, patchBody, removeAgent, removeGenerator, stateLabel, validate } from "./connectors.ts";

const connector: Connector = { id: "c1", kind: "vboard", name: "vboard", url: "https://vboard.example.com", board: "b_1", enabled: true, state: "connected", models: { chat: "qwen" } };

test("normalizeUrl", () => {
  assert.equal(normalizeUrl(" vboard.example.com/ "), "https://vboard.example.com");
  assert.equal(normalizeUrl("http://10.0.0.5:3000/"), "http://10.0.0.5:3000");
  assert.equal(normalizeUrl(""), "");
});

test("validate: a new connector needs an address and a token (any prefix: vboard's is configurable)", () => {
  assert.deepEqual(Object.keys(validate(emptyForm, false)).sort(), ["token", "url"]);
  assert.ok(validate({ ...emptyForm, url: "v.example", token: "vbp_ x" }, false).token);
  assert.ok(validate({ ...emptyForm, name: "x", url: "ftp://v.example", token: "vbp_x" }, false).url);
  assert.deepEqual(validate({ ...emptyForm, name: "x", url: "v.example", token: "vbp_x" }, false), {});
  assert.deepEqual(validate({ ...emptyForm, url: "v.example", token: "corp_x" }, false), {});
  // editing: a blank token keeps the stored one
  assert.deepEqual(validate({ ...formOf(connector) }, true), {});
});

test("createBody", () => {
  assert.deepEqual(createBody({ ...emptyForm, name: " vb ", url: "vboard.example.com", token: " vbp_x ", embed: "e" }), {
    kind: "vboard",
    name: "vb",
    url: "https://vboard.example.com",
    token: "vbp_x",
    models: { embed: "e" },
    generators: ["*"],
  });
  assert.equal(createBody({ ...emptyForm, name: "a", url: "b", token: "vbp_" }).models, undefined);
  // a blank name: the server's default
  assert.equal(createBody({ ...emptyForm, url: "b", token: "vbp_" }).name, undefined);
});

test("patchBody: only what changed", () => {
  assert.deepEqual(patchBody(connector, formOf(connector)), {});
  assert.deepEqual(patchBody(connector, { ...formOf(connector), name: "new", token: "vbp_new" }), { name: "new", token: "vbp_new" });
  assert.deepEqual(patchBody(connector, { ...formOf(connector), board: "" }), { board: "" });
  assert.deepEqual(patchBody(connector, { ...formOf(connector), classify: "laya" }), { models: { classify: "laya", embed: "", chat: "qwen" } });
  // the server's default name: a blank field is no change, clearing another name sends ""
  const dflt: Connector = { ...connector, name: "ai-connector" };
  assert.deepEqual(patchBody(dflt, { ...formOf(dflt), name: "" }), {});
  assert.deepEqual(patchBody(connector, { ...formOf(connector), name: "" }), { name: "" });
});

test("agents: ids derived and unique, ordering, the first must be a model", () => {
  assert.equal(agentId("claude-code/haiku"), "claude-code-haiku");
  assert.equal(agentId(" Qwen3.6-35B "), "qwen3.6-35b");
  assert.equal(agentKind("claude-code/opus"), "agent");
  assert.equal(agentKind("qwen3.6"), "model");
  let l = addAgent([], "qwen3.6");
  l = addAgent(l, "claude-code/haiku");
  assert.equal(addAgent(l, "qwen3.6"), l); // already there
  assert.deepEqual(
    l.map((a) => a.id),
    ["qwen3.6", "claude-code-haiku"],
  );
  // an id taken by another model's row gets a suffix
  assert.equal(addAgent([{ id: "x", model: "a", name: "" }], "X").at(-1)?.id, "x-2");
  assert.equal(agentsProblem(l), "");
  const up = moveAgent(l, 1, -1);
  assert.deepEqual(
    up.map((a) => a.model),
    ["claude-code/haiku", "qwen3.6"],
  );
  assert.match(agentsProblem(up), /must be a chat model/);
  assert.equal(moveAgent(l, 0, -1), l);
  assert.equal(moveAgent(l, 1, 1), l);
  assert.deepEqual(removeAgent(up, 0), [l[0]]);
  assert.match(agentsProblem([{ id: "a", model: "m", name: "" }, { id: "a", model: "n", name: "" }]), /twice/);
  assert.match(agentsProblem(Array.from({ length: 21 }, (_, i) => ({ id: `a${i}`, model: "m", name: "" }))), /At most 20/);
  assert.ok(validate({ ...emptyForm, url: "v.example", token: "vbp_x", agents: up }, false).agents);
});

test("agents in the bodies: sent when chosen or changed, [] back to the default", () => {
  const agents = addAgent(addAgent([], "qwen3.6"), "claude-code/haiku");
  agents[1] = { ...agents[1], name: " Claude " };
  assert.deepEqual(createBody({ ...emptyForm, url: "b", token: "vbp_", agents }).agents, [
    { id: "qwen3.6", model: "qwen3.6", name: "" },
    { id: "claude-code-haiku", model: "claude-code/haiku", name: "Claude" },
  ]);
  assert.equal(createBody({ ...emptyForm, url: "b", token: "vbp_" }).agents, undefined);
  const withAgents: Connector = { ...connector, agents: [{ id: "q", model: "qwen", name: "", kind: "model" }] };
  assert.deepEqual(patchBody(withAgents, formOf(withAgents)), {});
  assert.deepEqual(patchBody(withAgents, { ...formOf(withAgents), agents: [] }), { agents: [] });
  assert.deepEqual(patchBody(connector, { ...formOf(connector), agents: addAgent([], "qwen") }), { agents: [{ id: "qwen", model: "qwen", name: "" }] });
});

test("an agent's effort: kept through an edit, sent for Claude Code agents only", () => {
  const withEffort: Connector = {
    ...connector,
    agents: [
      { id: "q", model: "qwen", name: "", kind: "model" },
      { id: "op", model: "claude-code/opus", name: "", kind: "agent", effort: "high" },
    ],
  };
  const f = formOf(withEffort);
  assert.equal(f.agents[1].effort, "high");
  assert.deepEqual(patchBody(withEffort, f), {}); // nothing changed: the effort is not lost
  f.agents[1] = { ...f.agents[1], effort: "max" };
  f.agents[0] = { ...f.agents[0], effort: "low" }; // a model: not sent
  assert.deepEqual(patchBody(withEffort, f), {
    agents: [
      { id: "q", model: "qwen", name: "" },
      { id: "op", model: "claude-code/opus", name: "", effort: "max" },
    ],
  });
  f.agents[1] = { ...f.agents[1], effort: "" }; // back to the worker's default
  assert.deepEqual(patchBody(withEffort, f).agents?.[1], { id: "op", model: "claude-code/opus", name: "" });
});

test("Claude Code is no chat model: the form refuses it there", () => {
  assert.match(validate({ ...emptyForm, url: "https://b", token: "vbp_", chat: "claude-code/opus" }, false).chat ?? "", /Chat agents/);
  assert.equal(validate({ ...emptyForm, url: "https://b", token: "vbp_", chat: "qwen3.6-35b-a3b" }, false).chat, undefined);
});

test("generators: added once, moved, removed; sent when chosen or changed, [] turns them off", () => {
  let g = addGenerator(addGenerator([], "flux2-klein-4b"), "ltx-2.5-q4");
  assert.deepEqual(addGenerator(g, "ltx-2.5-q4"), g);
  g = moveGenerator(g, 1, -1);
  assert.deepEqual(g, ["ltx-2.5-q4", "flux2-klein-4b"]);
  assert.deepEqual(moveGenerator(g, 0, -1), g);
  assert.deepEqual(removeGenerator(g, 0), ["flux2-klein-4b"]);
  assert.deepEqual(createBody({ ...emptyForm, url: "b", token: "vbp_", generators: g }).generators, g);
  // a new connector: every generator model on disk; none when the list is emptied
  assert.deepEqual(createBody({ ...emptyForm, url: "b", token: "vbp_" }).generators, ["*"]);
  assert.equal(createBody({ ...emptyForm, url: "b", token: "vbp_", generators: [] }).generators, undefined);
  const withGenerators: Connector = { ...connector, generators: ["flux2-klein-4b"] };
  assert.deepEqual(formOf(withGenerators).generators, ["flux2-klein-4b"]);
  assert.deepEqual(patchBody(withGenerators, formOf(withGenerators)), {});
  assert.deepEqual(patchBody(withGenerators, { ...formOf(withGenerators), generators: [] }), { generators: [] });
  assert.deepEqual(patchBody(connector, { ...formOf(connector), generators: ["ltx-2.5-q4"] }), { generators: ["ltx-2.5-q4"] });
});

test("stateLabel", () => {
  assert.equal(stateLabel("connected").tone, "ok");
  assert.equal(stateLabel("connected", false).text, "disabled");
  assert.equal(stateLabel("error").tone, "bad");
  assert.equal(stateLabel("connecting").tone, "busy");
});
