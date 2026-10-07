# Web UI (web/): conventions

React 19 + TypeScript, Vite, Tailwind v4, shadcn/ui (`src/components/ui`, add more with
`pnpm dlx shadcn@latest add <name>`), Zustand, lucide icons. Built into `internal/webui/dist` (embedded in the hub
and `mlxlab serve`). `pnpm dev` (port 5173) proxies `/api` and `/files` to `MLXLAB_URL`.

## Structure

* `src/api/` — `types.ts` (the server's JSON), `client.ts` (`api.*`, typed errors: `OfflineError`, `SignInError`,
  `errorText(e)`).
* `src/store/app.ts` — `useApp`: `models`, `loaded`, `examples`, `prompts`, `jobs` (by id, kept live by the event
  stream), `put(job)`, `drop(id)`, `refreshModels()`, `refreshPrompts()`; `isActive(j)`, `jobsOf(jobs, kind)`.
  Select narrowly (`useApp((s) => s.jobs)` then `useMemo`), or with `useShallow` for derived arrays — never return
  a new object/array from a selector without `useShallow` (render loop).
* `src/store/player.ts` — `usePlayer` (the one audio player: `play(job, file?, at?)`, `toggle`, `seek`, `stop`),
  `usePlaying(file)`. The `PlayerBar` at the bottom of the shell shows it.
* `src/lib/` — `format.ts` (`duration`, `bytes`, `ago`, `took`, `progress`, `title`, `noun`, `isMovie`),
  `jobs.ts` (`submitEach(models, req)`, `waitJob(id)`, `runChat(model, system, user)`, `helperModel()`),
  `route.ts` (hash routes `#/<page>/<arg>`: `useRoute()`, `navigate(page, arg)`), `open.ts` (`openJob(job)`),
  `storage.ts` (`local.get/set`, never throws).
* `src/components/kit/` — shared building blocks; use them, extend them rather than copying:
  * `Layout.tsx`: `CreateLayout` (composer left with a pinned footer, results right; stacks on a phone),
    `Field`, `Advanced` (folded settings), `SectionHeader`, `Empty`.
  * `ModelPicker.tsx`: `useModelChoice(kind, scope?, filter?)` + `<ModelPicker choice=… />` (several ticked =
    a version with each).
  * `PromptBox.tsx`: a labelled textarea with ✨ Enhance (`enhance="enhance-image"` etc.) and Undo.
  * `Examples.tsx`: `ExamplesMenu`.
  * `JobBits.tsx`: `CreateButton` ("Create 2 versions"), `JobProgress`, `JobError` (Retry, Copy error),
    `StarButton` (acts on press), `DeleteJob` (confirm dialog; deletes files too).
* `src/features/<page>/` — one folder per page; `index.tsx` exports the page component(s) `({ arg })`.

## API notes

* Chat messages use capitalised keys: `{ Role: "user", Content: "…" }`.
* Media URLs are `/files/...` (the server rewrote paths); fine for `<img>`, `<video>`, `<audio>` (cookie auth).
* Uploads: `api.upload(file, { import?, folder? })` → `{ url, job? }`; reference images are passed as those urls.
* Job `request` fields: see `api/types.ts` (`Request`). A song version has `task`, `source`, `parent`.
* Chat sampling: `temperature`, `topP`, `topK`, `presencePenalty`, `maxTokens` are optional overrides; unset (or
  0 — JSON cannot tell 0 from unset for `temperature`) takes the model's default for thinking on or off, which
  `/api/models` lists per chat model as `sampling: {think, noThink}` (from the model's `generation_config.json` and
  Qwen's model-card recommendations, `internal/llm/sampling.go`). Greedy decoding is `greedy: true`. Qwen3.5/3.6:
  thinking 1.0 / top-p 0.95 / top-k 20 / presence 1.5 / 32768 tokens; without thinking 0.7 / 0.8 / 20 / 1.5 /
  4096. A low temperature without a presence penalty makes a thinking model think in circles (Qwen: "DO NOT use
  greedy decoding"). The chat settings popover shows these as "Auto"; older clients (the iOS app) still send
  `temperature` and `maxTokens`, which override only those two fields.
* Endpoints: see `internal/web/web.go` (worker) and `docs/architecture.md` (hub).

## Design

Calm, dense but airy, dark-first (light works too). Content first: big media, quiet chrome. Primary actions are
violet (`bg-primary`), everything else ghost/outline. Every list must stay stable while jobs update (keys by id;
no remounting rows). Work at phone width. Empty states say what to do. No native `alert/confirm/prompt`.
