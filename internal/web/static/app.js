// mlxlab web app: talks to the Go server (/api/…) and follows its jobs over server-sent events.
"use strict";

const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];
const h = (tag, attrs = {}, ...kids) => {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) el.append(k instanceof Node ? k : String(k));
  return el;
};
// 42 s, 2:05, 1:02:05
const dur = (sec) => {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return `${sec} s`;
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = String(sec % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
};
// replaceChildren that skips empty entries (null/false/undefined would otherwise show as text)
{
  const replace = Element.prototype.replaceChildren;
  Element.prototype.replaceChildren = function (...kids) { return replace.apply(this, kids.flat().filter((k) => k != null && k !== false)); };
}
const human = (n) => (n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : (n / (1 << 20)).toFixed(0) + " MB");
const store = {
  get(k, d) { try { const v = localStorage.getItem("mlxlab." + k); return v == null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem("mlxlab." + k, JSON.stringify(v)); } catch {} },
};

const state = { models: [], loaded: {}, jobs: new Map(), selected: store.get("selected", {}) };

async function api(path, opts = {}) {
  const r = await fetch(path, { headers: { "Content-Type": "application/json" }, ...opts });
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  return r.status === 204 ? null : r.json();
}
const submit = (req) => api("/api/jobs", { method: "POST", body: JSON.stringify(req) });

// ---------- tabs ----------
function showTab(name) {
  $$("#tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === name));
  $$(".tab").forEach((t) => t.classList.toggle("active", t.id === "tab-" + name));
  store.set("tab", name);
}
$$("#tabs button").forEach((b) => b.addEventListener("click", () => showTab(b.dataset.tab)));

// ---------- models ----------
async function loadModels() {
  const r = await api("/api/models");
  state.models = r.models;
  state.loaded = r.loaded;
  state.examples = r.examples || {};
  renderExamples();
  if (prompts.list.length) renderEnhanceBars();
  renderPickers();
  renderHeader();
}

function isLive(m) { return Object.values(state.loaded).some((v) => (v || "").split(", ").includes(m.id)); }

// renderPickers: one compact dropdown per kind — the button names the choice ("2 models" for more), the list has a
// checkbox per model with its state (in memory, on this Mac, to download) and size
function renderPickers() {
  for (const box of $$(".models")) {
    const kind = box.dataset.kind;
    const ms = state.models.filter((m) => m.kind === kind);
    let sel = state.selected[kind];
    if (!sel || !sel.some((id) => ms.find((m) => m.id === id))) {
      const first = ms.find((m) => m.here) || ms[0];
      sel = first ? [first.id] : [];
      state.selected[kind] = sel;
    }
    if (!ms.length) { box.replaceChildren(h("span", { class: "empty" }, "no models of this kind")); continue; }
    const chosen = ms.filter((m) => sel.includes(m.id));
    const open = box.classList.contains("open");
    const dot = (m) => h("span", { class: "dot" + (isLive(m) ? " live" : m.here ? " here" : "") });
    const button = h("button", { type: "button", class: "picker", onclick: (e) => { e.stopPropagation(); togglePicker(box); } },
      chosen.length === 1 ? [dot(chosen[0]), chosen[0].name] : chosen.length ? [h("span", { class: "dot live" }), `${chosen.length} models`] : "Choose a model",
      h("span", { class: "caret" }, "▾"));
    const list = h("div", { class: "picklist", onclick: (e) => e.stopPropagation() },
      h("div", { class: "pickhint" }, kind === "chat" ? "Each model gets its own column." : "Each model makes its own version."),
      ...ms.map((m) => {
        const on = sel.includes(m.id);
        return h("label", { class: "pick" + (on ? " on" : ""), title: m.about },
          h("input", { type: "checkbox", checked: on, onchange: (e) => toggleModel(kind, m.id, e.target.checked) }),
          dot(m),
          h("span", { class: "pname" }, m.name, h("small", {}, m.about)),
          h("span", { class: "size" }, isLive(m) ? "in memory" : m.here ? human(m.size) : "↓ " + human(m.size)));
      }));
    box.replaceChildren(button, open ? list : null);
  }
  renderChatCols();
  updateCreateButtons();
}

function togglePicker(box) {
  const open = !box.classList.contains("open");
  $$(".models.open").forEach((b) => b.classList.remove("open"));
  if (open) box.classList.add("open");
  renderPickers();
}
document.addEventListener("click", () => {
  if ($$(".models.open").length) { $$(".models.open").forEach((b) => b.classList.remove("open")); renderPickers(); }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && $$(".models.open").length) { $$(".models.open").forEach((b) => b.classList.remove("open")); renderPickers(); }
});

// the Create buttons say how many versions they make (one per selected model), and what downloads first
function updateCreateButtons() {
  const label = (kind, one) => {
    const n = (state.selected[kind] || []).length;
    return n > 1 ? `${one.startsWith("Generate movie") ? "Generate" : "Create"} ${n} ${one.includes("movie") ? "movies" : "versions"}` : one;
  };
  const note = (kind, anchor) => {
    if (!anchor) return;
    let el = anchor.parentElement.querySelector(`.create-note[data-kind="${kind}"]`);
    if (!el) {
      el = h("div", { class: "create-note", "data-kind": kind });
      anchor.insertAdjacentElement("afterend", el);
    }
    const sel = (state.selected[kind] || []).map((id) => state.models.find((m) => m.id === id)).filter(Boolean);
    const missing = sel.filter((m) => !m.here);
    const parts = [];
    if (sel.length > 1) parts.push(`One each with ${sel.map((m) => m.name).join(" and ")} — they run one after another.`);
    if (missing.length) parts.push(missing.map((m) => `${m.name} downloads ${human(m.size)} first.`).join(" "));
    el.textContent = parts.join(" ");
    el.hidden = !parts.length;
  };
  const img = $("#image-form button[type=submit]");
  if (img) { img.textContent = label("image", "Generate"); note("image", img); }
  const vid = $("#video-go");
  if (vid) { vid.textContent = label("video", movie.mode === "movie" ? "Generate movie" : "Generate"); note("video", vid); }
  const song = $("#music-form button[type=submit]");
  if (song) { song.textContent = label("music", "Create"); note("music", song); }
  const simple = $("#song-create-simple");
  if (simple) simple.textContent = label("music", "Create");
}

function toggleModel(kind, id, on) {
  let sel = state.selected[kind] || [];
  sel = on ? [...new Set([...sel, id])] : sel.filter((x) => x !== id);
  state.selected[kind] = sel;
  store.set("selected", state.selected);
  renderPickers();
}

// ---------- examples ----------
function renderExamples() {
  for (const sel of $$("select.examples")) {
    const ex = state.examples[sel.dataset.kind] || [];
    sel.replaceChildren(h("option", { value: "" }, "Examples…"), ...ex.map((e, i) => h("option", { value: i }, e.title)));
    sel.hidden = !ex.length;
  }
}
function setSelect(sel, value) {
  if (value && [...sel.options].some((o) => o.value === value)) sel.value = value;
}
$$("select.examples").forEach((sel) => sel.addEventListener("change", () => {
  const kind = sel.dataset.kind;
  const e = (state.examples[kind] || [])[+sel.value];
  sel.value = "";
  if (!e) return;
  switch (kind) {
    case "chat": $("#chat-input").value = e.prompt; $("#chat-input").focus(); break;
    case "image": $("#image-prompt").value = e.prompt; setSelect($("#image-size"), e.size); break;
    case "video":
      setSelect($("#video-size"), e.size);
      if (e.frames) $("#video-frames").value = e.frames;
      if (e.script && e.script.length) {
        setVideoMode("movie");
        $("#movie-shared").value = e.prompt || "";
        movie.clips = e.script.map((c) => ({ prompt: c.prompt, frames: c.frames || "", continue: !!c.continue }));
        renderClips();
      } else {
        setVideoMode("clip");
        $("#video-prompt").value = e.prompt;
      }
      break;
    case "embed": $("#embed-texts").value = e.texts.join("\n"); break;
    case "music":
      setSongMode("custom");
      $("#music-title").value = e.title.replace(/\s*\(.*\)$/, "");
      $("#music-style").value = e.prompt || "";
      $("#music-lyrics").value = e.lyrics || "";
      $("#music-instrumental").checked = !e.lyrics;
      if (e.duration) $("#music-duration").value = e.duration;
      updateInstrumental();
      break;
    case "classify":
      $("#classify-q").value = e.question;
      $("#classify-opts").value = e.options.map((o) => (o.description ? `${o.label}: ${o.description}` : o.label)).join("\n");
      $("#classify-texts").value = e.texts.join("\n");
      break;
  }
}));

function renderHeader() {
  const live = Object.entries(state.loaded).filter(([, v]) => v).map(([, v]) => v);
  $("#loaded").textContent = live.length ? "in memory: " + live.join(", ") : "";
  const active = [...state.jobs.values()].filter((j) => j.state === "queued" || j.state === "running");
  const run = active.find((j) => j.state === "running");
  $("#queue").textContent = run ? `running ${run.name}${active.length > 1 ? ` · ${active.length - 1} queued` : ""}` : "";
  $("#jobcount").textContent = active.length ? active.length : "";
}
$("#unload").addEventListener("click", async () => { await api("/api/unload", { method: "POST" }); loadModels(); });

// ---------- events ----------
function connect() {
  const es = new EventSource("/api/events");
  es.onmessage = (e) => {
    const u = JSON.parse(e.data);
    if (u.job) {
      const prev = state.jobs.get(u.jobId);
      state.jobs.set(u.jobId, u.job);
      if (prev && prev.state !== u.job.state && (u.job.state !== "running")) loadModels();
    } else if (u.event) {
      const j = state.jobs.get(u.jobId);
      if (!j) return;
      const ev = u.event;
      switch (ev.type) {
        case "status": j.status = ev.text; break;
        case "text": j.text = (j.text || "") + ev.text; break;
        case "progress": j.step = ev.step || 0; j.total = ev.total || 0; break;
        case "file": (j.files ||= []).push(ev.file); j.preview = ""; break;
        case "preview": j.preview = ev.file; j.previewClips = ev.step; break;
        case "log": (j.logs ||= []).push(ev.text); break;
        case "stats": j.stats = { ...(j.stats || {}), ...ev.data }; break;
        case "result": j.result = ev.data; break;
      }
    } else {
      state.jobs.delete(u.jobId);
    }
    scheduleRender();
  };
  es.onerror = () => { es.close(); setTimeout(() => { refreshJobs(); connect(); }, 1500); };
}

let pending = false;
function scheduleRender() {
  if (pending) return;
  pending = true;
  requestAnimationFrame(() => { pending = false; renderAll(); });
}
function renderAll() {
  renderHeader();
  renderChatCols();
  renderGallery("image");
  renderGallery("video");
  renderMusic();
  renderEmbed();
  renderClassify();
  renderJobs();
}

async function refreshJobs() {
  const js = await api("/api/jobs");
  state.jobs = new Map(js.map((j) => [j.id, j]));
  scheduleRender();
}

// ---------- dialogs ----------
// ask shows a dialog in the page: with `value` it asks for text (resolves to it, or null), otherwise it confirms
// (resolves true / false).
function ask({ title, text = "", value = null, placeholder = "", ok = "OK", danger = false }) {
  return new Promise((resolve) => {
    const input = value === null ? null : h("input", { value, placeholder });
    const close = (v) => { back.remove(); document.removeEventListener("keydown", key, true); resolve(v); };
    const done = () => close(input ? (input.value.trim() || null) : true);
    const key = (e) => { if (e.key === "Escape") { e.stopPropagation(); close(input ? null : false); } else if (e.key === "Enter") { e.preventDefault(); done(); } };
    const back = h("div", { class: "modal-back", onpointerdown: (e) => { if (e.target === back) close(input ? null : false); } },
      h("div", { class: "modal", role: "dialog" },
        h("h3", {}, title),
        text ? h("p", {}, text) : null,
        input,
        h("div", { class: "modal-acts" },
          h("button", { type: "button", class: "ghost", onclick: () => close(input ? null : false) }, "Cancel"),
          h("button", { type: "button", class: danger ? "danger" : "primary", onclick: done }, ok))));
    document.body.append(back);
    document.addEventListener("keydown", key, true);
    (input || back.querySelector(".modal-acts button:last-child")).focus();
    if (input) input.select();
  });
}

// toast shows a short message at the bottom (errors and notes; it never blocks the page)
function toast(msg) {
  let box = $("#toasts");
  if (!box) { box = h("div", { id: "toasts" }); document.body.append(box); }
  const t = h("div", { class: "toast", onclick: () => t.remove() }, String(msg));
  box.append(t);
  setTimeout(() => t.remove(), 6000);
}

// syncChildren puts nodes into box like replaceChildren, but a node already in its place stays attached: a button
// under the mouse that is detached and attached again loses the click.
function syncChildren(box, nodes) {
  nodes = nodes.flat().filter((n) => n != null && n !== false);
  nodes.forEach((n, i) => { if (box.children[i] !== n) box.insertBefore(n, box.children[i] || null); });
  while (box.children.length > nodes.length) box.lastElementChild.remove();
}

// ---------- delete, star ----------
const WHAT = { image: "image", video: "video", music: "song", chat: "answer", embed: "comparison", classify: "result" };

// deleteJob: the job and the files it made (after asking); true when it is gone
async function deleteJob(j) {
  const what = (j.request.script && j.request.script.length) ? "movie" : WHAT[j.kind] || "job";
  const files = (j.files && j.files.length) ? " and its file" : "";
  if (!await ask({ title: `Delete this ${what}${files}?`, text: (j.starred ? "It is starred. " : "") + "This cannot be undone.", ok: "Delete", danger: true })) return false;
  try {
    await api(`/api/jobs/${j.id}`, { method: "DELETE" });
    state.jobs.delete(j.id);
    scheduleRender();
    return true;
  } catch (err) { toast("Delete: " + err.message); return false; }
}

async function starJob(j, e) {
  if (e) e.stopPropagation();
  try {
    const n = await api(`/api/jobs/${j.id}/star`, { method: "POST", body: JSON.stringify({ starred: !j.starred }) });
    state.jobs.set(n.id, n);
    scheduleRender();
    if (j.kind === "music") renderDesk(true);
  } catch (err) { toast(err.message); }
}
const starButton = (j) => h("button", { class: "star" + (j.starred ? " on" : ""), title: j.starred ? "Unstar" : "Star",
  onpointerdown: (e) => { e.preventDefault(); starJob(state.jobs.get(j.id) || j, e); }, onclick: (e) => e.stopPropagation() }, j.starred ? "★" : "☆");

// ---------- errors ----------
// retryJob runs a failed job again; the new job takes the old one's place (chats, the open song)
async function retryJob(j) {
  try {
    const n = await api(`/api/jobs/${j.id}/retry`, { method: "POST" });
    state.jobs.delete(j.id);
    state.jobs.set(n.id, n);
    for (const conv of Object.values(chats)) for (const msg of conv) if (msg.job === j.id) msg.job = n.id;
    saveChats();
    if (studio.song === j.id) openSong(n.id);
    scheduleRender();
  } catch (err) { toast("Retry: " + err.message); }
}

async function copyText(text, btn) {
  try { await navigator.clipboard.writeText(text); } catch {
    const ta = h("textarea", {}, text); document.body.append(ta); ta.select(); document.execCommand("copy"); ta.remove();
  }
  if (btn) { const t = btn.textContent; btn.textContent = "Copied ✓"; setTimeout(() => (btn.textContent = t), 1400); }
}

// errorCard: what went wrong in words, what to do, the details folded away — Retry, Copy, Remove
function errorCard(j, compact) {
  const p = j.problem || { title: (j.error || "failed").split("\n")[0] };
  const detailed = j.error && j.error.trim() !== p.title;
  return h("div", { class: "errcard" + (compact ? " compact" : ""), onclick: (e) => e.stopPropagation() },
    h("div", { class: "errhead" }, h("span", { class: "erricon" }, "!"), h("b", {}, p.title)),
    p.hint ? h("div", { class: "errhint" }, p.hint) : null,
    detailed ? h("details", {}, h("summary", {}, "Details"), h("pre", {}, j.error)) : null,
    h("div", { class: "erracts" },
      h("button", { class: "small primary-ish", onclick: () => retryJob(j) }, "↻ Retry"),
      h("button", { class: "small", onclick: (e) => copyText(j.error || p.title, e.target) }, "Copy error"),
      h("button", { class: "small", onclick: () => deleteJob(j) }, "Delete")));
}

// ---------- progress ----------
function progress(j) {
  if (j.state === "failed") return errorCard(j);
  if (j.state === "canceled") return h("div", { class: "waiting" }, j.status || "canceled");
  const step = j.step || 0; // left out of the JSON when 0
  const pct = j.total ? Math.round((100 * step) / j.total) : null;
  const status = j.status || j.state;
  const download = /^fetching /.test(status); // step/total are MB then, and the status says the GB
  const meta = [pct != null ? (download ? `${pct}%` : `${step}/${j.total}`) : null,
    j.state === "running" ? dur((Date.now() - new Date(j.started)) / 1000) : null].filter(Boolean).join(" · ");
  return h("div", { class: "waiting" },
    h("div", { class: "line", title: status }, status),
    meta ? h("div", { class: "sub" }, meta) : null,
    h("div", { class: "bar" + (pct == null ? " indet" : "") }, h("i", { style: pct != null ? `width:${pct}%` : null })));
}
setInterval(() => { if ([...state.jobs.values()].some((j) => j.state === "running")) scheduleRender(); }, 1000);

// ---------- chat ----------
// every selected model has its own conversation: [{role, content} | {role: "assistant", job}]
const chats = store.get("chats", {});
const saveChats = () => store.set("chats", chats);

function renderMarkdownish(text) {
  // code blocks get <pre>; the rest stays plain text
  const out = [];
  const parts = text.split(/```[^\n]*\n?/);
  parts.forEach((p, i) => out.push(i % 2 ? h("pre", {}, p.replace(/\n$/, "")) : p));
  return out;
}

function assistantBody(text) {
  let think = null;
  let rest = text;
  const open = text.indexOf("<think>");
  if (open >= 0) {
    const close = text.indexOf("</think>");
    think = (close >= 0 ? text.slice(open + 7, close) : text.slice(open + 7)).trim();
    rest = close >= 0 ? text.slice(close + 8).replace(/^\s+/, "") : "";
  }
  return [think ? h("div", { class: "think" }, think) : null, ...renderMarkdownish(rest)];
}

function statsLine(j) {
  const s = j.stats || {};
  const bits = [];
  if (s.tokensPerSecond) bits.push(`${s.tokensPerSecond.toFixed(1)} tok/s`);
  if (s.tokens) bits.push(`${s.tokens} tokens`);
  if (s.promptTokens) bits.push(`prompt ${s.promptTokens}${s.cachedTokens ? ` (${s.cachedTokens} cached)` : ""}`);
  if (s.prefillPerSecond) bits.push(`prefill ${Math.round(s.prefillPerSecond)} tok/s`);
  if (s.firstTokenSeconds) bits.push(`first token ${s.firstTokenSeconds.toFixed(2)} s`);
  if (s.peakGB) bits.push(`peak ${s.peakGB.toFixed(1)} GB`);
  return bits.join(" · ");
}

function renderChatCols() {
  const cols = $("#chat-cols");
  const sel = state.selected.chat || [];
  if (!sel.length) { cols.replaceChildren(h("div", { class: "empty" }, "Pick one or more chat models above: each gets its own column, a message goes to all of them.")); return; }
  const keepScroll = new Map($$(".msgs", cols).map((m) => [m.dataset.id, m.scrollHeight - m.scrollTop - m.clientHeight < 40]));
  cols.replaceChildren(...sel.map((id) => {
    const m = state.models.find((x) => x.id === id);
    const conv = chats[id] || [];
    const last = [...conv].reverse().find((x) => x.job);
    const lj = last && state.jobs.get(last.job);
    const msgs = h("div", { class: "msgs", "data-id": id },
      conv.length ? conv.map((msg) => {
        if (msg.role === "user") return h("div", { class: "msg user" }, msg.content);
        const j = state.jobs.get(msg.job);
        if (!j) return h("div", { class: "msg assistant" }, msg.content || "…");
        if (j.state === "failed") return h("div", { class: "msg assistant" }, errorCard(j, true));
        const busy = j.state === "queued" || (j.state === "running" && !j.text);
        return h("div", { class: "msg assistant" },
          busy ? h("span", { class: "meta" }, j.status || j.state) : assistantBody(j.text || ""),
          !busy ? h("span", { class: "meta" }, j.state === "running" ? "…" : statsLine(j)) : null,
          j.state === "running" ? h("button", { class: "small", onpointerdown: (e) => { e.stopPropagation(); api(`/api/jobs/${j.id}/cancel`, { method: "POST" }); } }, "Stop") : null);
      }) : h("div", { class: "empty" }, m ? m.about : id));
    return h("div", { class: "col" },
      h("h3", {}, m ? m.name : id, h("span", {}, lj && lj.stats && lj.stats.tokensPerSecond ? `${lj.stats.tokensPerSecond.toFixed(1)} tok/s` : m && !m.here ? "↓ " + human(m.size) : "")),
      msgs);
  }));
  for (const m of $$(".msgs", cols)) if (keepScroll.get(m.dataset.id) !== false) m.scrollTop = m.scrollHeight;
}

function historyOf(id) {
  const sys = chatSystemText();
  const out = sys ? [{ Role: "system", Content: sys }] : [];
  for (const msg of chats[id] || []) {
    if (msg.role === "user") out.push({ Role: "user", Content: msg.content });
    else {
      const j = state.jobs.get(msg.job);
      let text = j ? j.text || "" : msg.content || "";
      const close = text.indexOf("</think>");
      if (close >= 0) text = text.slice(close + 8).trim(); // thoughts do not go back into the conversation
      if (text) out.push({ Role: "assistant", Content: text });
    }
  }
  return out;
}

$("#chat-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const text = $("#chat-input").value.trim();
  const sel = state.selected.chat || [];
  if (!text || !sel.length) return;
  $("#chat-input").value = "";
  for (const id of sel) {
    (chats[id] ||= []).push({ role: "user", content: text });
    try {
      const j = await submit({ model: id, messages: historyOf(id), temperature: +$("#chat-temp").value, maxTokens: +$("#chat-max").value, think: $("#chat-think").checked });
      state.jobs.set(j.id, j);
      chats[id].push({ role: "assistant", job: j.id });
    } catch (err) {
      chats[id].push({ role: "assistant", content: "⚠ " + err.message });
    }
  }
  saveChats();
  scheduleRender();
});
$("#chat-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("#chat-form").requestSubmit(); }
});
$("#chat-clear").addEventListener("click", () => {
  for (const id of state.selected.chat || []) delete chats[id];
  saveChats();
  renderChatCols();
});
// a reload keeps the conversation: answers are kept as text once their job is gone from the history
window.addEventListener("beforeunload", () => {
  for (const conv of Object.values(chats)) for (const msg of conv) {
    const j = msg.job && state.jobs.get(msg.job);
    if (j && j.text) msg.content = j.text;
  }
  saveChats();
});

// ---------- reference images ----------
const refs = { image: [], video: [] };
function setupRefs(kind) {
  const box = $(`#${kind}-refs`);
  const max = +box.dataset.max;
  const input = $("input", box);
  const add = async (files) => {
    for (const f of files) {
      if (refs[kind].length >= max) refs[kind].shift();
      const fd = new FormData();
      fd.append("file", f);
      const r = await fetch("/api/upload", { method: "POST", body: fd });
      if (!r.ok) { toast(await r.text()); continue; }
      refs[kind].push((await r.json()).url);
    }
    renderRefs(kind);
  };
  input.addEventListener("change", () => { add([...input.files]); input.value = ""; });
  box.addEventListener("dragover", (e) => { e.preventDefault(); box.classList.add("over"); });
  box.addEventListener("dragleave", () => box.classList.remove("over"));
  box.addEventListener("drop", (e) => { e.preventDefault(); box.classList.remove("over"); add([...e.dataTransfer.files].filter((f) => f.type.startsWith("image/"))); });
}
function renderRefs(kind) {
  const box = $(`#${kind}-refs`);
  $$(".ref", box).forEach((r) => r.remove());
  for (const url of refs[kind]) {
    box.prepend(h("div", { class: "ref" }, h("img", { src: url }),
      h("button", { type: "button", title: "remove", onclick: () => { refs[kind] = refs[kind].filter((u) => u !== url); renderRefs(kind); } }, "×")));
  }
}
function useAsRef(kind, url) {
  const max = +$(`#${kind}-refs`).dataset.max;
  refs[kind] = [...refs[kind], url].slice(-max);
  renderRefs(kind);
  showTab(kind);
  window.scrollTo({ top: 0, behavior: "smooth" });
}
setupRefs("image");
setupRefs("video");

// ---------- image & video ----------
const intOrNull = (v) => (v === "" || v == null ? undefined : +v);

$("#image-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const [w, hh] = $("#image-size").value.split("x").map(Number);
  for (const id of state.selected.image || []) {
    try {
      const j = await submit({ model: id, prompt: $("#image-prompt").value.trim(), width: w, height: hh,
        steps: intOrNull($("#image-steps").value), seed: intOrNull($("#image-seed").value), quantize: +$("#image-q").value,
        guidance: intOrNull($("#image-guidance").value), images: refs.image });
      state.jobs.set(j.id, j);
    } catch (err) { toast(err.message); }
  }
  scheduleRender();
});

$("#video-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const [w, hh] = $("#video-size").value.split("x").map(Number);
  const isMovie = movie.mode === "movie";
  const script = movie.clips.filter((c) => c.prompt.trim()).map((c, i) => ({
    prompt: c.prompt.trim(), frames: intOrNull(c.frames), continue: i > 0 && c.continue }));
  if (isMovie && !script.length) { toast("Add a clip with a description."); return; }
  if (!isMovie && !$("#video-prompt").value.trim()) { toast("Describe the video."); return; }
  for (const id of state.selected.video || []) {
    try {
      const j = await submit({ model: id, prompt: isMovie ? $("#movie-shared").value.trim() : $("#video-prompt").value.trim(),
        script: isMovie ? script : undefined, width: w, height: hh,
        frames: intOrNull($("#video-frames").value), fps: +$("#video-fps").value, seed: intOrNull($("#video-seed").value),
        lowRam: $("#video-lowram").checked, noAudio: $("#video-noaudio").checked, images: refs.video });
      state.jobs.set(j.id, j);
    } catch (err) { toast(err.message); }
  }
  scheduleRender();
});

// ---------- movie editor ----------
const movie = store.get("movie", { mode: "clip", clips: [{ prompt: "", frames: "", continue: false }] });
const saveMovie = () => store.set("movie", { mode: movie.mode, clips: movie.clips });

function setVideoMode(mode) {
  movie.mode = mode;
  $$("#video-mode button").forEach((b) => b.classList.toggle("on", b.dataset.mode === mode));
  $("#movie-editor").hidden = mode !== "movie";
  $("#video-prompt").hidden = mode === "movie";
  $("#video-frames-label").textContent = mode === "movie" ? "Frames per clip" : "Frames";
  updateCreateButtons();
  saveMovie();
  renderClips();
}
$$("#video-mode button").forEach((b) => b.addEventListener("click", () => setVideoMode(b.dataset.mode)));
$("#movie-add").addEventListener("click", () => {
  movie.clips.push({ prompt: "", frames: "", continue: true });
  renderClips();
  $$("#movie-clips textarea").pop()?.focus();
});

function renderClips() {
  const box = $("#movie-clips");
  box.replaceChildren(...movie.clips.map((c, i) => {
    const move = (d) => {
      const j = i + d;
      if (j < 0 || j >= movie.clips.length) return;
      [movie.clips[i], movie.clips[j]] = [movie.clips[j], movie.clips[i]];
      renderClips();
    };
    return h("div", { class: "clip" + (i > 0 && c.continue ? " cont" : "") },
      h("div", { class: "num" }, i + 1),
      h("div", { class: "body" },
        h("textarea", { rows: 2, placeholder: i === 0 ? "What happens in the first clip" : "What happens next",
          oninput: (e) => { c.prompt = e.target.value; saveMovie(); updateLength(); } }, c.prompt),
        h("div", { class: "bar2" },
          i === 0
            ? h("span", { class: "badge" }, "opening")
            : h("label", { class: "check" },
                h("input", { type: "checkbox", checked: c.continue, onchange: (e) => { c.continue = e.target.checked; renderClips(); } }),
                "continue from the last frame"),
          i > 0 && !c.continue ? h("span", { class: "badge" }, "new scene") : null,
          h("span", {}, "frames"),
          h("input", { type: "number", min: 9, step: 8, placeholder: "movie's", value: c.frames,
            oninput: (e) => { c.frames = e.target.value; saveMovie(); updateLength(); } }),
          h("span", { class: "push" },
            h("button", { type: "button", class: "small", title: "let a chat model write this clip out (with the shared description)",
              onclick: (e) => {
                const ta = e.target.closest(".body").querySelector("textarea");
                const p = prompts.list.find((x) => x.id === store.get("enhance.enhance-video", "")) || prompts.list.find((x) => x.use === "enhance-video");
                enhance(ta, p && p.id, e.target, null, $("#movie-shared").value.trim() ? "Characters and look of the whole film: " + $("#movie-shared").value.trim() + "\n\nThis shot:" : "");
              } }, "✨"),
            h("button", { type: "button", class: "small", title: "up", onclick: () => move(-1) }, "↑"),
            h("button", { type: "button", class: "small", title: "down", onclick: () => move(1) }, "↓"),
            h("button", { type: "button", class: "small", title: "remove",
              onclick: () => { movie.clips.splice(i, 1); if (!movie.clips.length) movie.clips.push({ prompt: "", frames: "", continue: false }); renderClips(); } }, "×")))));
  }));
  saveMovie();
  updateLength();
}

function updateLength() {
  const fps = +$("#video-fps").value || 24;
  const def = +$("#video-frames").value || 97;
  let frames = 0;
  movie.clips.forEach((c, i) => { const f = +c.frames || def; frames += i > 0 && c.continue ? f - 1 : f; });
  $("#movie-length").textContent = `${movie.clips.length} clips · about ${dur(frames / fps)} of film`;
}
$("#video-frames").addEventListener("input", updateLength);
$("#video-fps").addEventListener("input", updateLength);
setVideoMode(movie.mode);

const starredOnly = { image: store.get("starred.image", false), video: store.get("starred.video", false) };
function renderGallery(kind) {
  const box = $(`#${kind}-gallery`);
  // the filter above the gallery
  let bar = document.getElementById(kind + "-filter");
  if (!bar) {
    bar = h("div", { class: "galfilter", id: kind + "-filter" });
    box.before(bar);
  }
  const all = [...state.jobs.values()].filter((j) => j.kind === kind);
  const nStar = all.filter((j) => j.starred).length;
  bar.replaceChildren(
    h("span", { class: "folder" + (!starredOnly[kind] ? " on" : ""), onclick: () => { starredOnly[kind] = false; store.set("starred." + kind, false); renderGallery(kind); } }, `All ${all.length}`),
    h("span", { class: "folder" + (starredOnly[kind] ? " on" : ""), onclick: () => { starredOnly[kind] = true; store.set("starred." + kind, true); renderGallery(kind); } }, `★ Starred ${nStar}`));
  const js = all.filter((j) => !starredOnly[kind] || j.starred).reverse();
  if (!js.length) { box.replaceChildren(h("div", { class: "empty" }, starredOnly[kind] ? "Nothing starred yet — ☆ on a result." : kind === "image" ? "Images appear here." : "Videos appear here.")); return; }
  // keep playing videos alive: only rebuild cards whose job changed
  const old = new Map($$(".card", box).map((c) => [c.dataset.id, c]));
  syncChildren(box, js.map((j) => {
    const file = (j.files || [])[0];
    // a movie being made: its film so far plays (the card is not rebuilt each second, or the player would restart)
    const previewing = !file && j.preview;
    const sig = JSON.stringify([j.state, j.starred, previewing ? j.preview : [j.status, j.step], j.files, j.error,
      j.state === "running" && !previewing ? Math.floor(Date.now() / 1000) : 0]);
    const prev = old.get(j.id);
    if (prev && prev.dataset.sig === sig) {
      if (previewing) { const st = prev.querySelector(".pv-status"); if (st) st.textContent = previewStatus(j); }
      return prev;
    }
    const r = j.request || {};
    const s = j.stats || {};
    const media = file
      ? kind === "image"
        ? h("img", { src: file, loading: "lazy", onclick: () => zoom(file) })
        : h("video", { src: file, controls: true, loop: true, playsinline: true })
      : previewing
        ? h("div", { class: "preview" }, h("video", { src: j.preview, controls: true, playsinline: true }),
            h("div", { class: "pv-status" }, previewStatus(j)))
        : progress(j);
    const meta = [j.name, s.width && `${s.width}×${s.height}`, s.steps && `${s.steps} steps`, s.frames && `${s.frames} frames`,
      s.seed && `seed ${s.seed}`, j.state === "done" && s.seconds && (s.seconds < 60 ? `${s.seconds.toFixed(1)} s` : dur(s.seconds)), s.peakGB && `peak ${s.peakGB.toFixed(1)} GB`].filter(Boolean).join(" · ");
    const card = h("div", { class: "card", "data-id": j.id, "data-sig": sig },
      h("div", { class: "media" }, media),
      h("div", { class: "info" },
        r.script && r.script.length
          ? h("div", { class: "prompt", title: r.script.map((c, i) => `${i + 1}. ${c.prompt}`).join("\n") },
              h("span", { class: "badge" }, `movie · ${r.script.length} clips`), " ", r.script[0].prompt)
          : h("div", { class: "prompt", title: r.prompt }, r.prompt),
        h("div", { class: "meta" }, meta),
        h("div", { class: "actions" },
          file ? starButton(j) : null,
          j.state === "queued" || j.state === "running" ? h("button", { class: "small", onpointerdown: (e) => { e.stopPropagation(); api(`/api/jobs/${j.id}/cancel`, { method: "POST" }); } }, "Cancel") : null,
          file ? h("a", { class: "small", href: file, download: "" }, h("button", { class: "small" }, "Save")) : null,
          file && kind === "image" ? h("button", { class: "small", onclick: () => useAsRef("image", file) }, "Edit") : null,
          file && kind === "image" ? h("button", { class: "small", onclick: () => useAsRef("video", file) }, "Animate") : null,
          h("button", { class: "small", onclick: () => reuse(kind, j) }, "Reuse"),
          !(j.state === "queued" || j.state === "running") ? h("button", { class: "small", onclick: () => deleteJob(j) }, "Delete") : null)));
    return card;
  }));
}

function previewStatus(j) {
  const total = (j.request.script || []).length;
  return `preview: ${j.previewClips || (j.stats && j.stats.clipsDone) || "?"} of ${total} clips · ${j.status || ""}`;
}

function reuse(kind, j) {
  const r = j.request || {};
  if (kind === "video" && r.script && r.script.length) {
    setVideoMode("movie");
    $("#movie-shared").value = r.prompt || "";
    movie.clips = r.script.map((c) => ({ prompt: c.prompt, frames: c.frames || "", continue: !!c.continue }));
    renderClips();
  }
  $(`#${kind}-prompt`).value = r.prompt || "";
  if (r.seed) $(`#${kind}-seed`).value = (j.stats && j.stats.seed) || r.seed;
  else if (j.stats && j.stats.seed) $(`#${kind}-seed`).value = j.stats.seed;
  refs[kind] = (r.images || []).filter(Boolean);
  renderRefs(kind);
  window.scrollTo({ top: 0, behavior: "smooth" });
}

function zoom(src) {
  const lb = $("#lightbox");
  lb.replaceChildren(h("img", { src }));
  lb.hidden = false;
}
$("#lightbox").addEventListener("click", () => ($("#lightbox").hidden = true));
document.addEventListener("keydown", (e) => { if (e.key === "Escape") $("#lightbox").hidden = true; });

// ---------- music studio ----------
const TASKS = {
  remix: { task: "cover", label: "Remix", note: "A new style over this song: the structure and melody stay, the sound changes." },
  restyle: { task: "restyle", label: "Restyle melody", note: "YuE2 sings this song's melody score again in a new style — the tune and the lyrics stay. Edit the score below to change the melody itself." },
  replace: { task: "repaint", label: "Replace section", note: "Drag over the waveform to select a part; it is made again, the rest stays as it is." },
  extend: { task: "extend", label: "Extend", note: "Adds more music after the end, continuing the song." },
  lego: { task: "lego", label: "Add instrument", note: "Plays a new instrument track over the song (or the selected part)." },
  stems: { task: "extract", label: "Stems", note: "Pulls one instrument out of the mix as its own track." },
  vary: { task: "generate", label: "Variations", note: "The same song request again with a new seed." },
};
const TRACKS = ["vocals", "backing_vocals", "drums", "bass", "guitar", "keyboard", "percussion", "strings", "synth", "fx", "brass", "woodwinds"];
const studio = { song: store.get("studio.song", ""), tool: "remix", sel: null, peaks: {}, mode: store.get("studio.mode", "simple"),
  folder: store.get("studio.folder", ""), newFolders: store.get("studio.newFolders", []) };

const songs = () => [...state.jobs.values()].filter((j) => j.kind === "music").sort((a, b) => new Date(b.created) - new Date(a.created));
const songTitle = (j) => {
  const r = j.request || {};
  if (r.title) return r.title;
  const line = (r.lyrics || "").split("\n").map((l) => l.trim()).find((l) => l && !l.startsWith("["));
  return line || (r.prompt || "Untitled").split(",")[0];
};
const TASK_NAMES = { cover: "remix", restyle: "restyled", repaint: "new section", extend: "extended", lego: "+ instrument", extract: "stem" };
// a colour per song: FNV-1a over its id, spread over the hue circle
const hue = (id) => { let x = 2166136261; for (const c of id) { x ^= c.charCodeAt(0); x = Math.imul(x, 16777619) >>> 0; } return x % 360; };
const songArt = (j) => {
  if (j.cover) return `background: center / cover no-repeat url("${j.cover}")`;
  const h1 = hue(j.id), h2 = (h1 + 60) % 360;
  return `background: linear-gradient(135deg, hsl(${h1} 70% 55%), hsl(${h2} 70% 35%))`;
};
// the cover being made for a song (an image job), if any
const coverJob = (j) => [...state.jobs.values()].find((x) => x.request.coverFor === j.id && (x.state === "queued" || x.state === "running"));
const songFile = (j) => (j.files || [])[0];
const songLen = (j) => (j.stats && j.stats.duration) || (j.request && j.request.duration) || 0;

// instrumental: no lyrics to write
function updateInstrumental() {
  $("#music-lyrics").closest(".field").classList.toggle("off", $("#music-instrumental").checked);
}
$("#music-instrumental").addEventListener("change", updateInstrumental);

// fitLength estimates a song's length from its lyrics: a sung line is about two bars, sections without words get the
// length such parts usually have; the tempo comes from the style ("124 bpm"), else 110
function fitLength() {
  const style = $("#music-style").value;
  const bpm = +((style.match(/(\d{2,3})\s*bpm/i) || [])[1] || 110);
  const beatsFor = { intro: 16, outro: 16, break: 16, breakdown: 16, "build-up": 16, buildup: 16, drop: 32, inst: 16, instrumental: 32, interlude: 16, solo: 32 };
  let beats = 0;
  const lyrics = $("#music-instrumental").checked ? "" : $("#music-lyrics").value;
  for (const raw of lyrics.split("\n")) {
    const l = raw.trim();
    if (!l) continue;
    const tag = (l.match(/^\[([^\]]+)\]$/) || [])[1];
    if (tag) {
      const t = tag.toLowerCase().replace(/[^a-z-]/g, "").replace(/-?(short|medium|long)$/, "");
      beats += beatsFor[t] || 0;
    } else {
      beats += 8;
    }
  }
  if (!beats) beats = 64 * 4; // instrumental: about four 16-bar parts
  beats += 8; // a breath at the end
  const secs = Math.min(600, Math.max(20, Math.round((beats * 60) / bpm / 5) * 5));
  $("#music-duration").value = secs;
  const btn = $("#fit-length");
  btn.textContent = `≈ ${dur(secs)} at ${bpm} bpm`;
  setTimeout(() => (btn.textContent = "Fit to lyrics"), 2500);
}

// create: simple (a chat model writes the song) or custom
function setSongMode(mode) {
  studio.mode = mode;
  store.set("studio.mode", mode);
  $$("#song-mode button").forEach((b) => b.classList.toggle("on", b.dataset.mode === mode));
  $("#song-simple").hidden = mode !== "simple";
  $("#music-form").hidden = mode !== "custom";
}
$$("#song-mode button").forEach((b) => b.addEventListener("click", () => setSongMode(b.dataset.mode)));
setSongMode(studio.mode);

// writeSong asks a chat model for TITLE / STYLE / LYRICS and fills the custom form
async function writeSong(btn) {
  const idea = $("#song-idea").value.trim();
  const p = prompts.list.find((x) => x.use === "write-song");
  const model = enhanceModel();
  if (!idea || !p || !model) { toast(!idea ? "Describe the song first." : "No chat model or song prompt."); return false; }
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = "✍️ writing …";
  try {
    const want = $("#song-idea-inst").checked ? idea + "\n\n(instrumental, no vocals)" : idea;
    const j = await submit({ model, temperature: 0.8, maxTokens: 1200, messages: [{ Role: "system", Content: p.text }, { Role: "user", Content: want }] });
    state.jobs.set(j.id, j);
    const done = await waitJob(j.id);
    if (done.state !== "done") throw new Error(done.error || done.state);
    let text = done.text || "";
    const close = text.indexOf("</think>");
    if (close >= 0) text = text.slice(close + 8);
    const title = (text.match(/TITLE:\s*(.+)/i) || [])[1] || "";
    const style = (text.match(/STYLE:\s*(.+)/i) || [])[1] || "";
    const li = text.search(/LYRICS:\s*/i);
    const lyrics = li >= 0 ? text.slice(li).replace(/^LYRICS:\s*/i, "").trim() : "";
    $("#music-title").value = title.trim();
    $("#music-style").value = style.trim();
    $("#music-lyrics").value = lyrics;
    $("#music-instrumental").checked = $("#song-idea-inst").checked || !lyrics.replace(/\[[^\]]*\]/g, "").trim();
    updateInstrumental();
    fitLength(); // a written song: its length from its lyrics
    return true;
  } catch (err) {
    toast("Writing the song: " + err.message);
    return false;
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}
$("#song-write").addEventListener("click", async (e) => { if (await writeSong(e.target)) setSongMode("custom"); });
$("#song-create-simple").addEventListener("click", async (e) => { if (await writeSong(e.target)) $("#music-form").requestSubmit(); });

$("#music-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  for (const id of state.selected.music || []) {
    try {
      const j = await submit({ model: id, folder: (studio.folder && studio.folder !== "\u0000starred") ? studio.folder : undefined, title: $("#music-title").value.trim(), prompt: $("#music-style").value.trim(),
        lyrics: $("#music-lyrics").value.trim(), instrumental: $("#music-instrumental").checked,
        duration: +$("#music-duration").value || undefined, seed: intOrNull($("#music-seed").value) });
      state.jobs.set(j.id, j);
      openSong(j.id);
    } catch (err) { toast(err.message); }
  }
  scheduleRender();
});

// import a song: it joins the library and can be remixed
$("#song-import input").addEventListener("change", async (e) => {
  const f = e.target.files[0];
  e.target.value = "";
  if (!f) return;
  const fd = new FormData();
  fd.append("file", f);
  fd.append("import", "1");
  if (studio.folder && studio.folder !== "\u0000starred") fd.append("folder", studio.folder);
  const r = await fetch("/api/upload", { method: "POST", body: fd });
  if (!r.ok) { toast(await r.text()); return; }
  const { job } = await r.json();
  state.jobs.set(job.id, job);
  openSong(job.id);
  scheduleRender();
});
$("#song-search").addEventListener("input", () => renderLibrary());
$("#fit-length").addEventListener("click", fitLength);
$("#seed-random").addEventListener("click", () => { $("#music-seed").value = ""; $("#music-seed").focus(); });
updateInstrumental();

function openSong(id) {
  studio.song = id;
  studio.sel = null;
  store.set("studio.song", id);
  renderLibrary();
  renderDesk(true);
}

function renderMusic() {
  renderLibrary();
  renderDesk(false);
}

// ---------- song folders ----------
const folderOf = (j) => (j.request && j.request.folder) || "";
function allFolders() {
  const set = new Set(studio.newFolders);
  for (const j of songs()) {
    const parts = folderOf(j).split("/").filter(Boolean);
    for (let i = 1; i <= parts.length; i++) set.add(parts.slice(0, i).join("/"));
  }
  return [...set].sort((a, b) => a.localeCompare(b));
}
const inFolder = (j, f) => folderOf(j) === f || folderOf(j).startsWith(f + "/");
function openFolder(f) {
  studio.folder = f;
  store.set("studio.folder", f);
  renderLibrary();
}
async function moveSong(id, folder) {
  try {
    await api(`/api/jobs/${id}/folder`, { method: "POST", body: JSON.stringify({ folder }) });
    studio.newFolders = studio.newFolders.filter((x) => x !== folder);
    store.set("studio.newFolders", studio.newFolders);
  } catch (err) { toast("Move: " + err.message); }
}
async function newFolder(into) {
  const name = ((await ask({ title: "New folder", text: into ? `Inside “${into}”.` : "", value: "", placeholder: "Name", ok: "Create" })) || "").trim().replace(/\//g, "-");
  if (!name) return "";
  const f = into ? `${into}/${name}` : name;
  if (!studio.newFolders.includes(f)) studio.newFolders.push(f);
  store.set("studio.newFolders", studio.newFolders);
  return f;
}
async function renameFolder(f) {
  const leaf = f.split("/").pop();
  const name = ((await ask({ title: "Rename folder", text: "Its songs and folders come along.", value: leaf, ok: "Rename" })) || "").trim().replace(/\//g, "-");
  if (!name || name === leaf) return;
  const to = f.includes("/") ? f.slice(0, f.lastIndexOf("/") + 1) + name : name;
  await api("/api/folders/rename", { method: "POST", body: JSON.stringify({ from: f, to }) });
  studio.newFolders = studio.newFolders.map((x) => (x === f || x.startsWith(f + "/") ? to + x.slice(f.length) : x));
  store.set("studio.newFolders", studio.newFolders);
  openFolder(to);
}
async function removeFolder(f) {
  if (!await ask({ title: `Remove the folder “${f}”?`, text: "Its songs move up — nothing is deleted.", ok: "Remove" })) return;
  await api("/api/folders/rename", { method: "POST", body: JSON.stringify({ from: f, to: "" }) });
  studio.newFolders = studio.newFolders.filter((x) => x !== f && !x.startsWith(f + "/"));
  store.set("studio.newFolders", studio.newFolders);
  openFolder(f.includes("/") ? f.slice(0, f.lastIndexOf("/")) : "");
}

// the bar: where we are, the folders right inside it (drop a song on one to move it there)
function renderFolders() {
  const cur = studio.folder;
  const all = allFolders();
  const kids = all.filter((f) => (cur ? f.startsWith(cur + "/") && !f.slice(cur.length + 1).includes("/") : !f.includes("/")));
  const count = (f) => songs().filter((j) => inFolder(j, f)).length;
  const chip = (f, label, cls) => {
    const el = h("span", { class: "folder " + (cls || ""), onclick: () => openFolder(f) }, label, f !== cur ? h("span", { class: "n" }, String(f ? count(f) : songs().length)) : null);
    el.addEventListener("dragover", (e) => { e.preventDefault(); el.classList.add("drop"); });
    el.addEventListener("dragleave", () => el.classList.remove("drop"));
    el.addEventListener("drop", (e) => { e.preventDefault(); el.classList.remove("drop"); const id = e.dataTransfer.getData("text/song"); if (id) moveSong(id, f); });
    return el;
  };
  const crumbs = [];
  if (cur) {
    const parts = cur.split("/");
    crumbs.push(chip("", "All songs", "up"));
    parts.forEach((p, i) => crumbs.push(chip(parts.slice(0, i + 1).join("/"), "📁 " + p, i === parts.length - 1 ? "on" : "up")));
  } else {
    crumbs.push(chip("", "All songs", "on"));
  }
  const nStar = songs().filter((j) => j.starred).length;
  const starChip = h("span", { class: "folder" + (cur === "\u0000starred" ? " on" : ""), onclick: () => openFolder("\u0000starred") }, "★ Starred", h("span", { class: "n" }, String(nStar)));
  if (cur === "\u0000starred") {
    $("#song-folders").replaceChildren(chip("", "All songs", "up"), starChip);
    return;
  }
  $("#song-folders").replaceChildren(
    ...crumbs,
    starChip,
    ...kids.map((f) => chip(f, "📁 " + f.split("/").pop())),
    h("span", { class: "folder up", title: "New folder", onclick: () => { const f = newFolder(cur); if (f) openFolder(f); } }, "+ Folder"),
    cur ? h("span", { class: "folder up", title: "Rename this folder", onclick: () => renameFolder(cur) }, "✎") : null,
    cur ? h("span", { class: "folder up", title: "Remove this folder (its songs move up)", onclick: () => removeFolder(cur) }, "✕") : null);
}

function renderLibrary() {
  renderFolders();
  const q = $("#song-search").value.trim().toLowerCase();
  // in a folder: its songs (the ones in folders inside it show in those); searching looks everywhere
  const list = songs().filter((j) => (q ? true : studio.folder === "\u0000starred" ? j.starred : folderOf(j) === studio.folder))
    .filter((j) => !q || (songTitle(j) + " " + (j.request.prompt || "") + " " + (j.request.lyrics || "") + " " + folderOf(j)).toLowerCase().includes(q));
  const box = $("#song-list");
  if (!list.length) { box.replaceChildren(h("div", { class: "empty" }, q ? "No song matches." : studio.folder ? "No songs in this folder yet — drag one here, or make one while the folder is open." : "Your songs appear here.")); return; }
  // rows are kept unless something they show changed: a row rebuilt under the mouse loses the click on its buttons
  const old = new Map($$(".song", box).map((r) => [r.dataset.id, r]));
  syncChildren(box, list.map((j) => {
    const r = j.request || {};
    const active = j.state === "queued" || j.state === "running";
    const pct = active && j.total ? Math.round((100 * (j.step || 0)) / j.total) : 0;
    const file = songFile(j);
    const playing = player.file === file && !$("#pb-audio").paused;
    const sig = JSON.stringify([j.state, pct, j.status, file, songTitle(j), j.starred, j.cover, playing, j.id === studio.song, j.error]);
    const prev = old.get(j.id);
    if (prev && prev.dataset.sig === sig) return prev;
    const row = h("div", { class: "song" + (j.id === studio.song ? " on" : ""), draggable: "true", "data-id": j.id, "data-sig": sig, onclick: () => openSong(j.id),
      ondragstart: (e) => e.dataTransfer.setData("text/song", j.id) },
      h("div", { class: "art", style: songArt(j) }, active ? "…" : j.state === "failed" ? "!" : j.cover ? "" : "♪", active ? h("i", { class: "pg", style: `width:${pct}%` }) : null),
      h("div", { style: "min-width:0" },
        h("div", { class: "t" }, songTitle(j)),
        h("div", { class: "s" }, r.task && TASK_NAMES[r.task] ? h("span", { class: "tag" }, TASK_NAMES[r.task]) : null,
          active ? (j.status || j.state) : j.state === "failed" ? (j.problem && j.problem.title) || j.error : [j.name, songLen(j) ? dur(songLen(j)) : ""].filter(Boolean).join(" · "))),
      h("div", { class: "rowacts" },
        file ? starButton(j) : null,
        file ? h("button", { class: "round", title: "play", onclick: (e) => { e.stopPropagation(); play(j); } }, playing ? "❚❚" : "▶") : null));
    return row;
  }));
}

// the desk: one song, its waveform, the tools, the lyrics and the versions
let deskSig = "";
function renderDesk(force) {
  const box = $("#song-desk");
  const j = state.jobs.get(studio.song);
  if (!j) { box.replaceChildren(h("div", { class: "empty" }, "Pick a song to open it in the studio, or make one.")); deskSig = ""; return; }
  const cjs = coverJob(j);
  const sig = JSON.stringify([j.id, j.state, j.starred, j.step, j.status, songFile(j), songTitle(j), folderOf(j), j.cover || "", cjs ? [cjs.id, cjs.state, cjs.step] : "", allFolders().length, studio.tool, prompts.list.length, state.models.length]);
  if (!force && sig === deskSig) { updateWave(); return; }
  deskSig = sig;
  const r = j.request || {};
  const file = songFile(j);
  const parent = r.parent && state.jobs.get(r.parent);
  const cj = coverJob(j);
  box.replaceChildren(
    h("div", { class: "songhead" },
      h("div", { class: "cover", style: songArt(j) },
        cj ? h("div", { class: "covering" }, cj.state === "queued" ? "cover queued" : `cover · ${cj.total ? Math.round((100 * (cj.step || 0)) / cj.total) + " %" : cj.status || "…"}`) : null),
      h("div", { class: "covertools" },
        h("button", { class: "small", disabled: !!cj, onclick: (e) => generateCover(j, e.target) }, j.cover ? "🎨 New AI cover" : "🎨 AI cover"),
        h("label", { class: "small filebtn" }, "Use an image…", h("input", { type: "file", accept: "image/*", hidden: true, onchange: (e) => useCover(j, e.target) })),
        j.cover ? h("button", { class: "small", onclick: () => setCover(j, "") }, "Remove") : null)),
    h("div", { class: "row", style: "justify-content:space-between; align-items:center" },
      h("div", {}, songFile(j) ? starButton(j) : null, h("h2", { class: "rename", title: "Click to rename", onclick: (e) => renameSong(j, e.currentTarget) }, songTitle(j), h("span", { class: "pen" }, " ✎")),
        h("div", { class: "sub" }, [j.name, r.prompt, songLen(j) && dur(songLen(j)), j.stats && j.stats.seed && `seed ${j.stats.seed}`].filter(Boolean).join(" · "))),
      h("div", { class: "row" },
        h("select", { class: "small-select", title: "Move to a folder (its versions come along)", onchange: async (e) => {
            let f = e.target.value;
            if (f === "\u0000new") { f = newFolder(""); if (!f) { e.target.value = folderOf(j); return; } }
            await moveSong(j.id, f);
          } },
          h("option", { value: "", selected: !folderOf(j) }, "📁 No folder"),
          ...allFolders().map((f) => h("option", { value: f, selected: f === folderOf(j) }, "📁 " + f)),
          h("option", { value: "\u0000new" }, "New folder…")),
        j.model !== "upload" ? h("button", { class: "small", title: "Fill the Custom form with this song: change the seed, the model, the lyrics …", onclick: () => loadSong(j) }, "↺ Load into the form") : null,
        file ? h("a", { href: file, download: "" }, h("button", { class: "small" }, "Download")) : null,
        j.state === "queued" || j.state === "running"
          ? h("button", { class: "small", onpointerdown: (e) => { e.stopPropagation(); api(`/api/jobs/${j.id}/cancel`, { method: "POST" }); } }, "Cancel")
          : h("button", { class: "small", onclick: async () => { if (await deleteJob(j)) { studio.song = ""; renderMusic(); } } }, "Delete"))),
    parent ? h("div", { class: "sub" }, `${TASK_NAMES[r.task] || "version"} of `, h("a", { href: "#", onclick: (e) => { e.preventDefault(); openSong(parent.id); } }, songTitle(parent)),
      r.task === "repaint" || r.task === "lego" ? ` (${dur(r.start || 0)}–${r.end > 0 ? dur(r.end) : "end"})` : "") : null,
    file ? waveView(j) : progress(j),
    file ? h("div", { class: "tools" }, ...Object.entries(TASKS).map(([k, t]) => h("button", { class: k === studio.tool ? "on" : "", onclick: () => { studio.tool = k; renderDesk(true); } }, t.label))) : null,
    file ? toolView(j) : null,
    r.lyrics ? h("details", { open: true }, h("summary", { class: "sub" }, "Lyrics"), lyricsView(r.lyrics)) : null,
    versionsView(j));
  if (file) loadPeaks(j);
}

// loadSong fills the Custom form with a song's request (its model and the seed it used) to make it again changed
function loadSong(j) {
  const r = j.request || {};
  setSongMode("custom");
  $("#music-title").value = songTitle(j);
  $("#music-style").value = r.prompt || "";
  $("#music-lyrics").value = r.lyrics || "";
  $("#music-instrumental").checked = !!r.instrumental;
  updateInstrumental();
  if (r.duration) $("#music-duration").value = Math.round(r.duration);
  $("#music-seed").value = (j.stats && j.stats.seed) || r.seed || "";
  if (state.models.some((m) => m.id === j.model)) {
    state.selected.music = [j.model];
    store.set("selected", state.selected);
    renderPickers();
  }
  const f = $("#music-form");
  f.scrollIntoView({ behavior: "smooth", block: "start" });
  f.classList.add("flash");
  setTimeout(() => f.classList.remove("flash"), 1400);
  $("#music-seed").focus();
}

// ---------- covers ----------
async function setCover(j, image) {
  try {
    const n = await api(`/api/jobs/${j.id}/cover`, { method: "POST", body: JSON.stringify({ image }) });
    state.jobs.set(n.id, n);
    renderMusic();
  } catch (err) { toast("Cover: " + err.message); }
}

async function useCover(j, input) {
  const f = input.files[0];
  input.value = "";
  if (!f) return;
  const fd = new FormData();
  fd.append("file", f);
  const r = await fetch("/api/upload", { method: "POST", body: fd });
  if (!r.ok) { toast(await r.text()); return; }
  await setCover(j, (await r.json()).url);
}

// generateCover: a chat model turns the song into an image prompt ("Album cover" in Prompts), an image model paints
// it; the server makes the picture the song's cover when it is done
async function generateCover(j, btn) {
  const p = prompts.list.find((x) => x.use === "cover-art");
  const chat = enhanceModel();
  const images = state.models.filter((m) => m.kind === "image");
  const img = images.find((m) => (state.selected.image || []).includes(m.id)) || images.find((m) => m.here) || images[0];
  if (!p || !chat || !img) { toast("Needs a chat model, an image model and the “Album cover” prompt."); return; }
  const r = j.request || {};
  const about = [`Title: ${songTitle(j)}`, r.prompt && `Style: ${r.prompt}`, r.lyrics && `Lyrics:\n${r.lyrics}`].filter(Boolean).join("\n");
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = "🎨 writing the picture …";
  try {
    const w = await submit({ model: chat, temperature: 0.8, maxTokens: 400, messages: [{ Role: "system", Content: p.text }, { Role: "user", Content: about }] });
    state.jobs.set(w.id, w);
    const done = await waitJob(w.id);
    if (done.state !== "done") throw new Error(done.error || done.state);
    let text = done.text || "";
    const close = text.indexOf("</think>");
    if (close >= 0) text = text.slice(close + 8);
    text = text.trim().replace(/^["“]|["”]$/g, "");
    const c = await submit({ model: img.id, prompt: text, width: 1024, height: 1024, coverFor: j.id });
    state.jobs.set(c.id, c);
    scheduleRender();
  } catch (err) {
    toast("Cover: " + err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

// renameSong turns the title into a field: Enter or leaving it saves, Escape cancels
function renameSong(j, el) {
  if (el.querySelector("input")) return;
  const input = h("input", { value: songTitle(j), class: "title-input" });
  let done = false;
  const finish = async (save) => {
    if (done) return;
    done = true;
    const title = input.value.trim();
    if (save && title && title !== songTitle(j)) {
      try {
        const n = await api(`/api/jobs/${j.id}/title`, { method: "POST", body: JSON.stringify({ title }) });
        state.jobs.set(n.id, n);
      } catch (err) { toast("Rename: " + err.message); }
    }
    renderMusic();
    renderDesk(true);
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") finish(true);
    if (e.key === "Escape") finish(false);
  });
  input.addEventListener("blur", () => finish(true));
  el.replaceChildren(input);
  input.focus();
  input.select();
}

function lyricsView(text) {
  return h("div", { class: "lyricsview" }, ...text.split("\n").map((l) => (/^\s*\[.*\]\s*$/.test(l) ? h("div", { class: "sec" }, l) : h("div", {}, l || " "))));
}

// versions: the song's family (its root and everything made from it)
function versionsView(j) {
  const all = songs();
  let root = j;
  while (root.request.parent && state.jobs.get(root.request.parent)) root = state.jobs.get(root.request.parent);
  const kids = (id) => all.filter((x) => x.request.parent === id).reverse();
  const rows = [];
  const walk = (x, depth) => { rows.push([x, depth]); kids(x.id).forEach((k) => walk(k, depth + 1)); };
  walk(root, 0);
  if (rows.length < 2) return null;
  return h("details", { open: true }, h("summary", { class: "sub" }, `Versions (${rows.length})`),
    h("div", { class: "versions" }, ...rows.map(([x, d]) => h("div", { class: "v" + (x.id === j.id ? " on" : ""), style: `padding-left:${6 + d * 16}px`, onclick: () => openSong(x.id) },
      songFile(x) ? h("button", { class: "small", onclick: (e) => { e.stopPropagation(); play(x); } }, "▶") : h("span", { class: "meta" }, x.state),
      h("span", {}, d === 0 ? "original" : TASK_NAMES[x.request.task] || "variation"),
      h("span", { class: "meta" }, new Date(x.created).toLocaleTimeString())))));
}

// the waveform: peaks from the decoded file; drag to select, click to play from there
function waveView(j) {
  const wrap = h("div", { class: "wave", id: "wave" }, h("canvas", { id: "wave-canvas" }), h("div", { class: "sel", id: "wave-sel", hidden: true }),
    h("div", { class: "head", id: "wave-head" }), studio.peaks[j.id] ? null : h("div", { class: "loading" }, "reading the waveform …"));
  let down = null;
  const at = (e) => { const r = wrap.getBoundingClientRect(); return Math.min(1, Math.max(0, (e.clientX - r.left) / r.width)); };
  wrap.addEventListener("mousedown", (e) => { down = at(e); studio.sel = null; updateWave(); });
  window.addEventListener("mousemove", (e) => {
    if (down == null) return;
    const x = at(e);
    if (Math.abs(x - down) > 0.005) { studio.sel = [Math.min(down, x), Math.max(down, x)]; updateWave(); }
  });
  window.addEventListener("mouseup", (e) => {
    if (down == null) return;
    const len = songLen(j) || (studio.peaks[j.id] && studio.peaks[j.id].duration) || 0;
    if (!studio.sel) play(j, at(e) * len); // a click plays from there
    else if (studio.tool !== "lego") { studio.tool = "replace"; renderDesk(true); }
    down = null;
    updateTimes();
  }, { once: false });
  const len = () => (studio.peaks[j.id] && studio.peaks[j.id].duration) || songLen(j);
  const ticks = h("div", { class: "ruler" }, ...[0, 0.25, 0.5, 0.75, 1].map((f) => h("span", {}, dur(f * len()))));
  const times = h("span", { id: "sel-times" });
  return h("div", {}, wrap, ticks,
    h("div", { class: "transport" },
      h("button", { class: "small", onclick: () => play(j) }, "▶ / ❚❚"),
      h("button", { class: "small", onclick: () => { if (studio.sel) play(j, studio.sel[0] * len(), studio.sel[1] * len()); } }, "Play selection"),
      h("button", { class: "small", onclick: () => { studio.sel = null; updateWave(); updateTimes(); } }, "Clear selection"),
      times));
}

function updateTimes() {
  const j = state.jobs.get(studio.song);
  const el = $("#sel-times");
  if (!j || !el) return;
  const len = (studio.peaks[j.id] && studio.peaks[j.id].duration) || songLen(j);
  el.textContent = studio.sel ? `selected ${dur(studio.sel[0] * len)} – ${dur(studio.sel[1] * len)}` : "drag over the waveform to select a part";
  const s = $("#tool-start"), e = $("#tool-end");
  if (s && e && studio.sel) { s.value = (studio.sel[0] * len).toFixed(1); e.value = (studio.sel[1] * len).toFixed(1); }
}

async function loadPeaks(j) {
  if (studio.peaks[j.id]) { drawWave(j); return; }
  try {
    const buf = await (await fetch(songFile(j))).arrayBuffer();
    const ctx = new (window.AudioContext || window.webkitAudioContext)();
    const audio = await ctx.decodeAudioData(buf);
    ctx.close();
    const ch = audio.getChannelData(0), n = 900, step = Math.max(1, Math.floor(ch.length / n));
    const peaks = new Float32Array(n);
    for (let i = 0; i < n; i++) {
      let m = 0;
      for (let k = i * step; k < Math.min(ch.length, (i + 1) * step); k += 8) m = Math.max(m, Math.abs(ch[k]));
      peaks[i] = m;
    }
    studio.peaks[j.id] = { peaks, duration: audio.duration };
  } catch {
    studio.peaks[j.id] = { peaks: new Float32Array(0), duration: songLen(j) };
  }
  if (studio.song === j.id) renderDesk(true);
}

function drawWave(j) {
  const c = $("#wave-canvas");
  const p = studio.peaks[j.id];
  if (!c || !p) return;
  const r = c.getBoundingClientRect(), dpr = window.devicePixelRatio || 1;
  c.width = r.width * dpr;
  c.height = r.height * dpr;
  const g = c.getContext("2d");
  const accent = getComputedStyle(document.documentElement).getPropertyValue("--accent").trim() || "#7c5cff";
  g.fillStyle = accent;
  const n = p.peaks.length, w = c.width / Math.max(1, n), mid = c.height / 2;
  for (let i = 0; i < n; i++) {
    const v = Math.max(1 * dpr, p.peaks[i] * mid * 0.95);
    g.fillRect(i * w, mid - v, Math.max(1, w - 0.5 * dpr), v * 2);
  }
  updateWave();
  updateTimes();
}

function updateWave() {
  const j = state.jobs.get(studio.song);
  const sel = $("#wave-sel"), head = $("#wave-head");
  if (!j || !sel) return;
  sel.hidden = !studio.sel;
  if (studio.sel) { sel.style.left = studio.sel[0] * 100 + "%"; sel.style.width = (studio.sel[1] - studio.sel[0]) * 100 + "%"; }
  const a = $("#pb-audio");
  const len = (studio.peaks[j.id] && studio.peaks[j.id].duration) || a.duration || songLen(j);
  head.style.display = player.file === songFile(j) && len ? "" : "none";
  if (len) head.style.left = Math.min(100, (a.currentTime / len) * 100) + "%";
}

// the tools: each makes a new version (a job with this song as its source and parent)
function toolView(j) {
  const t = TASKS[studio.tool];
  const r = j.request || {};
  const capable = t.task === "generate" ? state.models.filter((m) => m.kind === "music") : state.models.filter((m) => m.kind === "music" && (m.tasks || []).includes(t.task));
  if (studio.tool === "vary" && j.model === "upload") return h("div", { class: "tool" }, h("div", { class: "note" }, "An imported song has no request to vary — try Remix."));
  if (!capable.length) return h("div", { class: "tool" }, h("div", { class: "note" }, t.note), h("div", { class: "note" }, `No model here can do this yet (ACE-Step 1.5 can — it is being added).`));
  const pickKey = "studio.model." + t.task;
  const model = h("select", { onchange: (e) => store.set(pickKey, e.target.value) },
    ...capable.map((m) => h("option", { value: m.id, selected: m.id === (store.get(pickKey, "") || (capable.find((x) => x.id === j.model) || capable[0]).id) }, m.name)));
  const style = h("textarea", { rows: 2, placeholder: "Style for the new version" }, studio.tool === "remix" ? "" : r.prompt || "");
  const lyrics = h("textarea", { rows: 4, placeholder: "Lyrics (optional)" }, r.lyrics || "");
  const len = () => (studio.peaks[j.id] && studio.peaks[j.id].duration) || songLen(j);
  const fields = [];
  const extra = {};
  if (studio.tool === "remix") {
    const strength = h("input", { type: "range", min: 0.1, max: 1, step: 0.05, value: 0.7 });
    const sv = h("span", { class: "meta" }, "0.70");
    strength.addEventListener("input", () => (sv.textContent = (+strength.value).toFixed(2)));
    fields.push(h("label", {}, "New style", style), h("label", {}, h("span", {}, "Keep of the original ", sv), strength), h("label", {}, "Lyrics", lyrics));
    extra.get = () => ({ strength: +strength.value, prompt: style.value.trim(), lyrics: lyrics.value.trim() });
  } else if (studio.tool === "restyle") {
    const score = j.stats && j.stats.score;
    if (!score) return h("div", { class: "tool" }, h("div", { class: "note" }, t.note),
      h("div", { class: "note" }, "This song has no melody score — only songs made by YuE2 have one. Make one with YuE2 first, or use Remix."));
    const abc = h("textarea", { rows: 10, class: "mono", placeholder: "loading the score …" });
    let original = "";
    fetch(score).then((r) => r.text()).then((x) => { original = x; abc.value = x; });
    const reset = h("button", { type: "button", class: "small", onclick: () => (abc.value = original) }, "Reset score");
    fields.push(h("label", {}, "New style", style), h("label", {}, "Lyrics", lyrics),
      h("label", {}, h("span", {}, "Melody score (ABC notation) ", reset), abc));
    extra.get = () => ({ prompt: style.value.trim(), lyrics: lyrics.value.trim(), abc: abc.value.trim() === original.trim() ? "" : abc.value });
  } else if (studio.tool === "replace" || studio.tool === "lego") {
    const start = h("input", { id: "tool-start", type: "number", step: 0.5, min: 0, value: studio.sel ? (studio.sel[0] * len()).toFixed(1) : 0 });
    const end = h("input", { id: "tool-end", type: "number", step: 0.5, min: 0, value: studio.sel ? (studio.sel[1] * len()).toFixed(1) : len().toFixed(1) });
    fields.push(h("div", { class: "row" }, h("label", {}, "From (s)", start), h("label", {}, "To (s)", end)));
    if (studio.tool === "lego") {
      const track = h("select", {}, ...TRACKS.map((x) => h("option", { value: x }, x.replace("_", " "))));
      fields.push(h("label", {}, "Instrument", track), h("label", {}, "How it should sound", style));
      extra.get = () => ({ start: +start.value, end: +end.value, track: track.value, prompt: style.value.trim() });
    } else {
      fields.push(h("label", {}, "Style for this part", style), h("label", {}, "Lyrics for this part", lyrics));
      extra.get = () => ({ start: +start.value, end: +end.value, prompt: style.value.trim(), lyrics: lyrics.value.trim() });
    }
  } else if (studio.tool === "extend") {
    const add = h("input", { type: "number", min: 5, max: 240, step: 5, value: 30 });
    fields.push(h("label", {}, "Seconds to add", add), h("label", {}, "Style", style), h("label", {}, "Lyrics for the new part", h("textarea", { rows: 3, id: "ext-lyrics", placeholder: "[verse] …" })));
    extra.get = () => ({ extend: +add.value, prompt: style.value.trim(), lyrics: ($("#ext-lyrics").value || "").trim() });
  } else if (studio.tool === "stems") {
    const track = h("select", {}, ...TRACKS.map((x) => h("option", { value: x }, x.replace("_", " "))));
    fields.push(h("label", {}, "Pull out", track));
    extra.get = () => ({ track: track.value });
  } else {
    const n = h("input", { type: "number", min: 1, max: 4, value: 2 });
    fields.push(h("label", {}, "How many", n));
    extra.get = () => ({ count: +n.value });
  }
  const go = h("button", { class: "primary", onclick: async () => {
    const x = extra.get();
    try {
      if (studio.tool === "vary") {
        for (let i = 0; i < x.count; i++) {
          const v = await submit({ ...r, model: model.value, seed: undefined, source: undefined, parent: j.id, task: undefined, title: songTitle(j) });
          state.jobs.set(v.id, v);
        }
      } else {
        const v = await submit({ model: model.value, task: t.task, source: songFile(j), parent: j.id, title: songTitle(j),
          duration: songLen(j) || undefined, prompt: x.prompt ?? r.prompt, lyrics: x.lyrics ?? undefined, instrumental: r.instrumental,
          start: x.start, end: x.end, strength: x.strength, track: x.track, extend: x.extend, abc: x.abc || undefined });
        state.jobs.set(v.id, v);
        openSong(v.id);
      }
      scheduleRender();
    } catch (err) { toast(err.message); }
  } }, studio.tool === "vary" ? "Make variations" : `${t.label} →`);
  return h("div", { class: "tool" }, h("div", { class: "note" }, t.note), h("label", {}, "Model", model), ...fields, go);
}

// the player bar
const player = { file: "", job: "", stopAt: 0 };
function play(j, from, to) {
  const a = $("#pb-audio");
  const file = songFile(j);
  if (!file) return;
  if (player.file === file && from == null) {
    if (a.paused) a.play(); else a.pause();
    return;
  }
  if (player.file !== file) { a.src = file; player.file = file; }
  player.job = j.id;
  player.stopAt = to || 0;
  $("#playerbar").hidden = false;
  $("#pb-title").textContent = songTitle(j);
  $("#pb-sub").textContent = [j.name, (j.request || {}).prompt].filter(Boolean).join(" · ");
  const start = () => { if (from != null) a.currentTime = from; a.play(); };
  if (a.readyState >= 1) start(); else a.addEventListener("loadedmetadata", start, { once: true });
}
{
  const a = $("#pb-audio");
  $("#pb-play").addEventListener("click", () => (a.paused ? a.play() : a.pause()));
  $("#pb-seek").addEventListener("input", (e) => { if (a.duration) a.currentTime = (e.target.value / 1000) * a.duration; });
  const tick = () => {
    $("#pb-play").textContent = a.paused ? "▶" : "❚❚";
    $("#pb-time").textContent = dur(a.currentTime || 0).replace(/\.\d s$/, " s");
    $("#pb-dur").textContent = a.duration ? dur(a.duration) : "";
    if (a.duration) $("#pb-seek").value = Math.round((a.currentTime / a.duration) * 1000);
    if (player.stopAt && a.currentTime >= player.stopAt) { a.pause(); player.stopAt = 0; }
    updateWave();
  };
  // the position only moves the bar and the playhead; the library is rebuilt when playing starts or stops (rebuilding
  // it on every position update replaced the pause button under the mouse, and the click got lost)
  a.addEventListener("timeupdate", tick);
  for (const ev of ["play", "pause", "ended", "loadedmetadata"]) a.addEventListener(ev, () => { tick(); renderLibrary(); });
}
window.addEventListener("resize", () => { const j = state.jobs.get(studio.song); if (j) drawWave(j); });

// ---------- embed ----------
let embedJob = null;
$("#embed-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const texts = $("#embed-texts").value.split("\n").map((s) => s.trim()).filter(Boolean);
  const id = (state.selected.embed || [])[0];
  if (!id || texts.length < 2) return;
  const j = await submit({ model: id, texts });
  state.jobs.set(j.id, j);
  embedJob = j.id;
  scheduleRender();
});
function renderEmbed() {
  const out = $("#embed-out");
  const j = embedJob ? state.jobs.get(embedJob) : [...state.jobs.values()].filter((x) => x.kind === "embed").pop();
  if (!j) { out.replaceChildren(); return; }
  if (!j.result) { out.replaceChildren(progress(j)); return; }
  const { similarity, texts, dim } = j.result;
  const cell = (v, i, k) => {
    const a = i === k ? 0 : Math.max(0, (v - 0.3) / 0.7);
    return h("td", { class: "v", style: `background: rgba(124, 92, 255, ${(a * 0.85).toFixed(2)})` }, v.toFixed(2));
  };
  out.replaceChildren(
    h("div", { class: "stats" }, `${texts.length} texts · ${dim} dimensions · ${(j.stats.seconds * 1000).toFixed(0)} ms`),
    h("table", { class: "heat" },
      h("tr", {}, h("th", { class: "t" }, ""), texts.map((_, i) => h("th", {}, "#" + (i + 1)))),
      similarity.map((row, i) => h("tr", {}, h("td", { class: "t" }, `#${i + 1} ${texts[i]}`), row.map((v, k) => cell(v, i, k))))));
}

// ---------- classify ----------
let classifyJob = null;
$("#classify-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const options = $("#classify-opts").value.split("\n").map((s) => s.trim()).filter(Boolean).map((line) => {
    const i = line.indexOf(":");
    return i > 0 ? { label: line.slice(0, i).trim(), description: line.slice(i + 1).trim() } : { label: line };
  });
  const states = $("#classify-texts").value.split("\n").map((s) => s.trim()).filter(Boolean);
  const id = (state.selected.classify || [])[0];
  if (!id || !states.length || !options.length) return;
  const j = await submit({ model: id, instructions: $("#classify-q").value, options, states });
  state.jobs.set(j.id, j);
  classifyJob = j.id;
  scheduleRender();
});
function renderClassify() {
  const out = $("#classify-out");
  const j = classifyJob ? state.jobs.get(classifyJob) : [...state.jobs.values()].filter((x) => x.kind === "classify").pop();
  if (!j) { out.replaceChildren(); return; }
  if (!j.result) { out.replaceChildren(progress(j)); return; }
  out.replaceChildren(
    h("div", { class: "stats" }, `${j.result.answers.length} answers in ${(j.stats.seconds * 1000).toFixed(0)} ms`),
    h("table", {}, h("tr", {}, h("th", {}, "Text"), h("th", {}, "Answer"), h("th", {}, "Confidence"), h("th", {}, "Probabilities")),
      j.result.answers.map((a) => h("tr", {},
        h("td", {}, a.text), h("td", {}, h("b", {}, a.choice)),
        h("td", {}, h("span", { class: "conf", style: `width:${Math.round(a.confidence * 80)}px` }), (a.confidence * 100).toFixed(0) + "%"),
        h("td", { class: "meta" }, Object.entries(a.probabilities).sort((x, y) => y[1] - x[1]).map(([k, v]) => `${k} ${(v * 100).toFixed(0)}%`).join(" · "))))));
}

// ---------- jobs ----------
const canOpen = (j) => (j.files && j.files.length) || j.preview || j.kind === "chat" || j.result;

// openJob goes to a job's result: the image or video in its gallery, the song in the studio, the chat, the table
function openJob(j) {
  switch (j.kind) {
    case "image":
    case "video": {
      showTab(j.kind);
      requestAnimationFrame(() => {
        const card = document.querySelector(`#${j.kind}-gallery .card[data-id="${j.id}"]`);
        if (!card) return;
        card.scrollIntoView({ behavior: "smooth", block: "center" });
        card.classList.add("flash");
        setTimeout(() => card.classList.remove("flash"), 1600);
        if (j.kind === "image" && j.files && j.files[0]) zoom(j.files[0]);
      });
      break;
    }
    case "music":
      showTab("music");
      openSong(j.id);
      break;
    case "chat": {
      showTab("chat");
      const col = document.querySelector(`.msgs[data-id="${j.model}"]`);
      if (!col && !(state.selected.chat || []).includes(j.model)) { toggleModel("chat", j.model, true); }
      break;
    }
    case "embed": embedJob = j.id; showTab("embed"); renderEmbed(); break;
    case "classify": classifyJob = j.id; showTab("classify"); renderClassify(); break;
  }
}
const openLogs = new Set();
function renderJobs() {
  const box = $("#jobs-list");
  const js = [...state.jobs.values()].reverse();
  if (!js.length) { box.replaceChildren(h("div", { class: "empty" }, "No jobs yet.")); return; }
  const summary = (j) => {
    const r = j.request || {};
    if (j.kind === "chat") { const u = (r.messages || []).filter((m) => m.Role === "user").pop(); return u ? u.Content : ""; }
    if (j.kind === "embed") return `${(r.texts || []).length} texts`;
    if (j.kind === "classify") return r.instructions;
    if (j.kind === "music") return (r.task ? (TASK_NAMES[r.task] || r.task) + ": " : "") + songTitle(j);
    return r.prompt;
  };
  box.replaceChildren(h("table", { class: "jobs" },
    h("tr", {}, h("th", {}, "When"), h("th", {}, "Model"), h("th", {}, "Request"), h("th", {}, "State"), h("th", {}, "Time"), h("th", {}, "")),
    js.flatMap((j) => {
      const row = h("tr", {},
        h("td", { class: "meta" }, new Date(j.created).toLocaleTimeString()),
        h("td", {}, j.name),
        h("td", { class: "req" }, (summary(j) || "").slice(0, 120)),
        h("td", { class: "st", title: j.error || j.status }, h("span", { class: "state " + j.state }, j.state), " ", h("span", { class: "meta" }, j.state === "running" ? j.status : (j.problem && j.problem.title) || j.error || "")),
        h("td", { class: "meta num" }, j.started ? dur(((j.finished ? new Date(j.finished) : Date.now()) - new Date(j.started)) / 1000) : ""),
        h("td", { class: "acts" },
          canOpen(j) ? h("button", { class: "small", onclick: () => openJob(j) }, "Open") : null, " ",
          j.state === "failed" || j.state === "canceled" ? h("button", { class: "small", onclick: () => retryJob(j) }, "↻ Retry") : null, " ",
          j.state === "failed" ? h("button", { class: "small", onclick: (e) => copyText(j.error || "", e.target) }, "Copy error") : null, " ",
          h("button", { class: "small", onclick: () => { openLogs.has(j.id) ? openLogs.delete(j.id) : openLogs.add(j.id); renderJobs(); } }, "Log"), " ",
          j.state === "queued" || j.state === "running"
            ? h("button", { class: "small", onpointerdown: (e) => { e.stopPropagation(); api(`/api/jobs/${j.id}/cancel`, { method: "POST" }); } }, "Cancel")
            : h("button", { class: "small", onclick: () => deleteJob(j) }, "Delete")));
      if (!openLogs.has(j.id)) return [row];
      return [row, h("tr", {}, h("td", { colspan: 6 }, h("pre", { class: "logs" }, (j.logs || []).join("\n") || "(no log)")))];
    })));
}

// ---------- system prompts ----------
const USE_NAMES = { chat: "Chat system prompts", "enhance-image": "Enhance: image", "enhance-video": "Enhance: video", "enhance-music": "Enhance: music" };
const prompts = { list: [], selected: store.get("prompt.selected", "chat-assistant"), chat: store.get("chat.sysprompt", "") };

async function loadPrompts() {
  const r = await api("/api/prompts");
  prompts.list = r.prompts;
  renderPromptList();
  renderPromptEditor();
  renderChatPrompts();
  renderEnhanceBars();
}

// chat: none, a library prompt, or your own text
function renderChatPrompts() {
  const sel = $("#chat-sysprompt");
  const chat = prompts.list.filter((p) => p.use === "chat");
  sel.replaceChildren(h("option", { value: "" }, "(none)"),
    ...chat.map((p) => h("option", { value: p.id }, p.name + (p.modified ? " (edited)" : ""))),
    h("option", { value: "custom" }, "Custom text…"));
  if (![...sel.options].some((o) => o.value === prompts.chat)) prompts.chat = "";
  sel.value = prompts.chat;
  $("#chat-system").hidden = prompts.chat !== "custom";
}
$("#chat-sysprompt").addEventListener("change", (e) => {
  prompts.chat = e.target.value;
  store.set("chat.sysprompt", prompts.chat);
  $("#chat-system").hidden = prompts.chat !== "custom";
});
$("#chat-system").value = store.get("chat.system", "");
$("#chat-system").addEventListener("input", (e) => store.set("chat.system", e.target.value));
$("#chat-sysedit").addEventListener("click", () => {
  if (prompts.chat && prompts.chat !== "custom") prompts.selected = prompts.chat;
  renderPromptList(); renderPromptEditor(); showTab("prompts");
});
function chatSystemText() {
  if (prompts.chat === "custom") return $("#chat-system").value.trim();
  const p = prompts.list.find((x) => x.id === prompts.chat);
  return p ? p.text : "";
}

function renderPromptList() {
  const box = $("#prompt-list");
  box.replaceChildren(h("div", { class: "plist" }, Object.keys(USE_NAMES).flatMap((use) => {
    const ps = prompts.list.filter((p) => p.use === use);
    return [h("h4", {}, USE_NAMES[use]), ...ps.map((p) => h("div", { class: "pitem" + (p.id === prompts.selected ? " on" : ""),
      onclick: () => { prompts.selected = p.id; store.set("prompt.selected", p.id); renderPromptList(); renderPromptEditor(); } },
      h("span", {}, p.name),
      h("span", { class: "tags" }, p.builtin ? (p.modified ? h("span", { class: "badge" }, "edited") : null) : h("span", { class: "badge" }, "yours"))))];
  })));
}

function renderPromptEditor(draft) {
  const box = $("#prompt-edit");
  const p = draft || prompts.list.find((x) => x.id === prompts.selected) || prompts.list[0];
  if (!p) { box.replaceChildren(); return; }
  const isNew = !p.id;
  const name = h("input", { value: p.name, placeholder: "Name" });
  const use = h("select", { disabled: p.builtin }, ...Object.entries(USE_NAMES).map(([v, t]) => h("option", { value: v, selected: v === p.use }, t)));
  const text = h("textarea", { placeholder: "The system prompt — paste one from another model, or write your own" }, p.text);
  const words = h("span", { class: "src" });
  const count = () => (words.textContent = `${text.value.trim().split(/\s+/).filter(Boolean).length} words`);
  text.addEventListener("input", count);
  count();
  const save = async (asNew) => {
    try {
      const body = { id: asNew ? "" : p.id, name: asNew && !isNew ? name.value + " (copy)" : name.value, use: use.value, text: text.value };
      const saved = await api("/api/prompts", { method: "POST", body: JSON.stringify(body) });
      prompts.selected = saved.id;
      store.set("prompt.selected", saved.id);
      await loadPrompts();
    } catch (err) { toast(err.message); }
  };
  box.replaceChildren(
    h("div", { class: "row3" }, h("label", { style: "flex:1" }, "Name", name), h("label", {}, "For", use)),
    p.source ? h("div", { class: "src" }, "Default from: " + p.source) : null,
    text,
    h("div", { class: "row3" },
      h("button", { class: "primary", onclick: () => save(false) }, isNew ? "Create" : "Save"),
      !isNew ? h("button", { class: "ghost", onclick: () => save(true) }, "Duplicate") : null,
      p.builtin && p.modified ? h("button", { class: "ghost", onclick: async () => {
        if (!await ask({ title: "Put this prompt back to its default?", ok: "Reset" })) return;
        await api(`/api/prompts/${p.id}`, { method: "DELETE" }); await loadPrompts(); } }, "Reset to default") : null,
      !p.builtin && !isNew ? h("button", { class: "ghost", onclick: async () => {
        if (!await ask({ title: `Delete “${p.name}”?`, ok: "Delete", danger: true })) return;
        await api(`/api/prompts/${p.id}`, { method: "DELETE" }); prompts.selected = "chat-assistant"; await loadPrompts(); } }, "Delete") : null,
      words),
    p.builtin && p.modified ? h("details", { class: "default" }, h("summary", {}, "Show the default"), h("pre", {}, p.default)) : null);
}
$("#prompt-new").addEventListener("click", () => {
  const use = (prompts.list.find((x) => x.id === prompts.selected) || {}).use || "chat";
  renderPromptEditor({ id: "", name: "", use, text: "", builtin: false });
});

// ---------- enhance: a chat model rewrites a short idea with an enhance prompt ----------
const enhanceModel = () => {
  const chats = state.models.filter((m) => m.kind === "chat");
  const pick = store.get("enhance.model", "");
  return (chats.find((m) => m.id === pick) || chats.find(isLive) || chats.find((m) => m.here) || chats[0] || {}).id;
};

function renderEnhanceBars() {
  for (const bar of $$(".enhance")) {
    const use = bar.dataset.use;
    const ps = prompts.list.filter((p) => p.use === use);
    const key = "enhance." + use;
    const sel = h("select", { title: "the system prompt that does the rewriting", onchange: (e) => store.set(key, e.target.value) },
      ...ps.map((p) => h("option", { value: p.id, selected: p.id === store.get(key, "") }, p.name)));
    const models = h("select", { title: "the chat model that rewrites", onchange: (e) => store.set("enhance.model", e.target.value) },
      ...state.models.filter((m) => m.kind === "chat").map((m) => h("option", { value: m.id, selected: m.id === enhanceModel() }, m.name)));
    const target = $("#" + bar.dataset.target);
    const btn = h("button", { type: "button", class: "enh-go", title: "Let a chat model write this out", onclick: () => enhance(target, sel.value, btn, undo) }, "✨ Enhance");
    const undo = h("button", { type: "button", class: "enh-undo", hidden: true, onclick: () => { target.value = undo.dataset.before; undo.hidden = true; } }, "↺ Undo");
    const opts = h("details", {}, h("summary", { title: "Which model, which prompt" }, "⚙"),
      h("div", { class: "enh-pop" }, h("label", {}, "Model", models), h("label", {}, "Prompt", sel)));
    bar.replaceChildren(btn, undo, opts);
  }
}

// enhance rewrites target's text; the answer replaces it (Undo brings the old text back)
async function enhance(target, promptID, btn, undo, context) {
  const text = target.value.trim();
  const p = prompts.list.find((x) => x.id === promptID);
  const model = enhanceModel();
  if (!text || !p || !model) return;
  btn.disabled = true;
  btn.textContent = "✨ working …";
  try {
    const j = await submit({ model, temperature: 0.7, maxTokens: 900,
      messages: [{ Role: "system", Content: p.text }, { Role: "user", Content: context ? `${context}\n\n${text}` : text }] });
    state.jobs.set(j.id, j);
    const done = await waitJob(j.id);
    if (done.state !== "done") throw new Error(done.error || done.state);
    let out = done.text || "";
    const close = out.indexOf("</think>");
    if (close >= 0) out = out.slice(close + 8);
    out = out.trim().replace(/^["“]|["”]$/g, "").trim();
    if (out) {
      if (undo) { undo.dataset.before = target.value; undo.hidden = false; }
      target.value = out;
      target.dispatchEvent(new Event("input"));
    }
  } catch (err) {
    toast("Enhance: " + err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "✨ Enhance";
  }
}
// a click elsewhere closes an open ⚙ popover
document.addEventListener("click", (e) => { $$(".enhance details[open]").forEach((d) => { if (!d.contains(e.target)) d.open = false; }); });

// waitJob resolves when a job has finished (the event stream keeps state.jobs current)
function waitJob(id) {
  return new Promise((resolve) => {
    const t = setInterval(() => {
      const j = state.jobs.get(id);
      if (j && !(j.state === "queued" || j.state === "running")) { clearInterval(t); resolve(j); }
    }, 300);
  });
}

// ---------- start ----------
// /#prompts opens that tab (bookmarks, links), /#music/<song> that song in the studio; else the last tab
{
  const [tab, arg] = location.hash.slice(1).split("/");
  showTab((tab && document.getElementById("tab-" + tab) && tab) || store.get("tab", "chat"));
  if (tab === "music" && arg) studio.song = arg;
}
loadModels().then(refreshJobs).then(connect).then(loadPrompts);
