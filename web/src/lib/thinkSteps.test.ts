/// <reference types="node" />
import assert from "node:assert/strict";
import { test } from "node:test";
import { partsOf, roundStarts, shapeOf, stepsOf, thinkFor, thoughtSeconds, wordsOf, type Piece } from "./thinkSteps.ts";

const think = (text: string, open = false) => ({ kind: "think", text, open });
const words = (text: string) => ({ kind: "text", text });

test("a plain answer, one thought, a thought the prompt opened", () => {
  assert.deepEqual(partsOf("Hello."), [words("Hello.")]);
  assert.deepEqual(partsOf("<think>\nhmm\n</think>\n\nHello."), [think("hmm"), words("Hello.")]);
  assert.deepEqual(partsOf("hmm, so\n</think>\n\nHello."), [think("hmm, so"), words("Hello.")]);
  assert.deepEqual(partsOf(""), []);
  assert.deepEqual(partsOf(undefined), []);
});

test("every round of a chat with tools: many thoughts, words between them", () => {
  const t = "<think>\nI need the file.</think>\n\nLet me make it.<think>\nIt worked.\n</think>\n\nHere is **hello.pdf**.";
  assert.deepEqual(partsOf(t), [think("I need the file."), words("Let me make it."), think("It worked."), words("Here is **hello.pdf**.")]);
  // thoughts back to back (no words before the call)
  assert.deepEqual(partsOf("<think>\nA</think>\n\n<think>\nB</think>\n\nDone."), [think("A"), think("B"), words("Done.")]);
  assert.equal(wordsOf(t), "Let me make it.\n\nHere is **hello.pdf**.");
});

test("Qwen's empty thought (think off) is no step", () => {
  assert.deepEqual(partsOf("<think>\n\n</think>\n\nHi."), [words("Hi.")]);
  assert.deepEqual(partsOf("<think>\n\n</think>\n\nLet me look.<think>\n\n</think>\n\nDone."), [words("Let me look."), words("Done.")]);
  assert.deepEqual(partsOf("\n\n</think>\n\nHi."), [words("Hi.")]);
});

test("a thought still going (live, or out of tokens) and tags still arriving", () => {
  assert.deepEqual(partsOf("<think>"), [think("", true)]);
  assert.deepEqual(partsOf("<think>\nfirst I"), [think("first I", true)]);
  assert.deepEqual(partsOf("<think>\nA</think>\n\nok<think>\nB is"), [think("A"), words("ok"), think("B is", true)]);
  // a closing tag half there is not shown
  assert.deepEqual(partsOf("<think>\nalmost</thi"), [think("almost", true)]);
  // an opening tag half there neither
  assert.deepEqual(partsOf("<think>\nA</think>\n\nok<thi"), [think("A"), words("ok")]);
});

test("markdown and html in a thought stay text (the page renders them as muted, escaped markdown)", () => {
  assert.deepEqual(partsOf("<think>\n- **plan**: <b>x</b>\n</think>ok"), [think("- **plan**: <b>x</b>"), words("ok")]);
});

test("steps: pieces of text and calls, thoughts numbered across the turn", () => {
  const pieces: Piece<string>[] = [
    { kind: "text", key: "t0", text: "<think>\nneed a file</think>\n\n" },
    { kind: "tool", key: "c1", call: "make_file" },
    { kind: "text", key: "t1", text: "<think>\nmade it</think>\n\nLet me check.<think>\nlooks fine</think>" },
    { kind: "tool", key: "c2", call: "files_list" },
    { kind: "text", key: "t2", text: "<think>\nok</think>\n\nDone: hello.pdf" },
  ];
  const steps = stepsOf(pieces);
  assert.deepEqual(
    steps.map((s) => (s.kind === "tool" ? `tool:${s.call}` : s.kind === "think" ? `think${s.n}:${s.text}` : `text:${s.text}`)),
    ["think0:need a file", "tool:make_file", "think1:made it", "text:Let me check.", "think2:looks fine", "tool:files_list", "think3:ok", "text:Done: hello.pdf"],
  );
  assert.equal(new Set(steps.map((s) => s.key)).size, steps.length, "keys are unique");
  assert.deepEqual(shapeOf(steps), { thoughts: 4, tools: 2, answered: true, inThought: false, multi: true });
});

test("shapes: thinking without calls, out of tokens, a plain answer", () => {
  const one = stepsOf([{ kind: "text", key: "t0", text: "<think>\nx</think>\n\nAnswer." }]);
  assert.deepEqual(shapeOf(one), { thoughts: 1, tools: 0, answered: true, inThought: false, multi: false });
  const cut = stepsOf([
    { kind: "text", key: "t0", text: "<think>\nx</think>" },
    { kind: "tool", key: "c", call: 1 },
    { kind: "text", key: "t1", text: "<think>\nstill thinking when the tokens ran" },
  ]);
  assert.deepEqual(shapeOf(cut), { thoughts: 2, tools: 1, answered: false, inThought: true, multi: true });
  assert.deepEqual(shapeOf(stepsOf([{ kind: "text", key: "t0", text: "Just words." }])), { thoughts: 0, tools: 0, answered: true, inThought: false, multi: false });
  assert.deepEqual(shapeOf(stepsOf([])), { thoughts: 0, tools: 0, answered: false, inThought: false, multi: false });
});

test("round starts: where each round after a call began (think on)", () => {
  const t = "<think>\nA</think>\n\nok<think>\nB</think>\n\n<think>\nC";
  assert.deepEqual(roundStarts(t), [t.indexOf("<think>\nB"), t.indexOf("<think>\nC")]);
  assert.deepEqual(roundStarts("<think>\nA</think>\n\nanswer"), []);
  assert.deepEqual(roundStarts("no thinking <think> mentioned"), []);
});

test("thought seconds: from the speed, else from the whole time", () => {
  // 200 tokens at 50 tok/s = 4 s of generating; the thought is half the text
  assert.equal(thoughtSeconds({ chars: 485, allChars: 1000, tokens: 200, tokensPerSecond: 50, first: false }), 2);
  assert.equal(thoughtSeconds({ chars: 485, allChars: 1000, tokens: 200, tokensPerSecond: 50, firstToken: 1, first: true }), 3);
  assert.equal(thoughtSeconds({ chars: 985, allChars: 1000, total: 11, firstToken: 1, first: true }), 11);
  assert.equal(thoughtSeconds({ chars: 10, allChars: 100, first: true }), undefined);
});

test("Think: on for tools by default, the plain chat's old setting, the user's choice per zone", () => {
  assert.equal(thinkFor("files", {}), true);
  assert.equal(thinkFor("web", {}), true);
  assert.equal(thinkFor("home", {}, false), true);
  assert.equal(thinkFor("", {}), false);
  assert.equal(thinkFor("", {}, true), true);
  assert.equal(thinkFor("files", { files: false }), false);
  assert.equal(thinkFor("web", { files: false }), true);
  assert.equal(thinkFor("", { chat: false }, true), false);
  assert.equal(thinkFor("", { chat: true }), true);
});
