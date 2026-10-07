#!/usr/bin/env bash
# Builds LeVo2.cpp (https://github.com/ckadirt/LeVo2.cpp, the ggml port of Tencent's LeVo 2 / SongGeneration v2)
# with the Metal backend and prints the directory holding levo-cli and levo-render.
#
#   levo2_build.sh <target-dir> <commit>
#
# Clones (with the ggml submodule) into <target-dir>/src, applies a small Metal patch (see below), builds into <target-dir>/src/build and prints
# <target-dir>/src/build/bin as the last line of stdout. Idempotent: a finished build (marked by
# <target-dir>/.built-<commit>) is not repeated; an interrupted one resumes. Everything except the last line goes to
# stderr. The binaries link their dylibs via an rpath into the build tree, so leave the tree in place.
#
# License: LeVo2.cpp and the SongGeneration weights are for academic, research and education use only; commercial and
# production use is prohibited.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 <target-dir> <commit>" >&2
  exit 2
fi
target=$1
commit=$2
repo=https://github.com/ckadirt/LeVo2.cpp.git
src="$target/src"
bin="$src/build/bin"
marker="$target/.built-$commit"

if [ -f "$marker" ] && [ -x "$bin/levo-cli" ] && [ -x "$bin/levo-render" ]; then
  echo "$bin"
  exit 0
fi

cmake=$(command -v cmake || true)
[ -n "$cmake" ] || cmake=/opt/homebrew/bin/cmake
[ -x "$cmake" ] || { echo "cmake not found (brew install cmake)" >&2; exit 1; }
command -v git >/dev/null || { echo "git not found (xcode-select --install)" >&2; exit 1; }

mkdir -p "$target"
export GIT_TERMINAL_PROMPT=0
{
  if [ ! -d "$src/.git" ]; then
    rm -rf "$src"
    git clone --quiet "$repo" "$src"
  fi
  cd "$src"
  git cat-file -e "$commit^{commit}" 2>/dev/null || git fetch --quiet origin
  git -c advice.detachedHead=false checkout --quiet --force "$commit"
  git reset --quiet --hard "$commit"
  git submodule update --init --recursive --force --quiet
  # Metal has no DIAG_MASK_INF (the LeLM aborts with "unsupported op" in the prefill); build the causal mask from
  # supported ops instead. On CPU the patched build produces bit-identical tokens to the unpatched one.
  git apply --whitespace=nowarn <<'PATCH'
diff --git a/src/levo-kv.cpp b/src/levo-kv.cpp
index 3dcb5ff..574e0d8 100644
--- a/src/levo-kv.cpp
+++ b/src/levo-kv.cpp
@@ -19,6 +19,20 @@
 namespace levo::detail {
 namespace {
 
+// mlxlab patch: ggml's Metal backend has no DIAG_MASK_INF, so the causal mask is built from ops every backend has
+// (arange, repeat, sub, step, scale, add). Masked scores get -1e30, which softmax turns into exactly 0, as with -inf.
+ggml_tensor * causal_mask(ggml_context * context, ggml_tensor * scores, int n_past) {
+    const int64_t keys = scores->ne[0];
+    const int64_t queries = scores->ne[1];
+    ggml_tensor * key_index = ggml_arange(context, 0.0F, static_cast<float>(keys), 1.0F);
+    ggml_tensor * query_index = ggml_arange(context, static_cast<float>(n_past),
+                                            static_cast<float>(n_past + queries), 1.0F);
+    key_index = ggml_repeat(context, ggml_reshape_2d(context, key_index, keys, 1),
+                            ggml_new_tensor_2d(context, GGML_TYPE_F32, keys, queries));
+    ggml_tensor * ahead = ggml_step(context, ggml_sub(context, key_index, ggml_reshape_2d(context, query_index, 1, queries)));
+    return ggml_add(context, scores, ggml_scale(context, ahead, -1e30F));
+}
+
 // This is deliberately independent of graph_capacity in levo-lm.cpp.  The
 // cached graph has two write nodes per transformer layer in addition to the
 // ordinary forward nodes.
@@ -425,7 +439,7 @@ ggml_tensor * cached_llama_tower(ggml_context * context, ggml_cgraph * graph,
             capture_last_heads(context, first_layer_ops, "attention_scores", attention, token_count,
                                emulate_fp16_activations);
         }
-        attention = ggml_diag_mask_inf(context, attention, past);
+        attention = causal_mask(context, attention, static_cast<int>(past));
         attention = ggml_soft_max_ext(context, attention, nullptr,
                                       emulate_fp16_activations ? 1.0F
                                                                : 1.0F / std::sqrt(static_cast<float>(head_dimension)),
diff --git a/src/levo-lm.cpp b/src/levo-lm.cpp
index eef66ec..eb7ef55 100644
--- a/src/levo-lm.cpp
+++ b/src/levo-lm.cpp
@@ -17,6 +17,20 @@
 namespace levo::detail {
 namespace {
 
+// mlxlab patch: ggml's Metal backend has no DIAG_MASK_INF, so the causal mask is built from ops every backend has
+// (arange, repeat, sub, step, scale, add). Masked scores get -1e30, which softmax turns into exactly 0, as with -inf.
+ggml_tensor * causal_mask(ggml_context * context, ggml_tensor * scores, int n_past) {
+    const int64_t keys = scores->ne[0];
+    const int64_t queries = scores->ne[1];
+    ggml_tensor * key_index = ggml_arange(context, 0.0F, static_cast<float>(keys), 1.0F);
+    ggml_tensor * query_index = ggml_arange(context, static_cast<float>(n_past),
+                                            static_cast<float>(n_past + queries), 1.0F);
+    key_index = ggml_repeat(context, ggml_reshape_2d(context, key_index, keys, 1),
+                            ggml_new_tensor_2d(context, GGML_TYPE_F32, keys, queries));
+    ggml_tensor * ahead = ggml_step(context, ggml_sub(context, key_index, ggml_reshape_2d(context, query_index, 1, queries)));
+    return ggml_add(context, scores, ggml_scale(context, ahead, -1e30F));
+}
+
 constexpr std::size_t graph_capacity = 4096;
 
 struct context_deleter {
@@ -182,7 +196,7 @@ ggml_tensor * llama_tower(ggml_context * context, ggml_tensor * input, ggml_tens
         q = ggml_permute(context, q, 0, 2, 1, 3); // [head_dim, tokens, heads]
         k = ggml_permute(context, k, 0, 2, 1, 3); // [head_dim, tokens, heads]
         ggml_tensor * attention = ggml_mul_mat(context, k, q); // [keys, queries, heads]
-        attention = ggml_diag_mask_inf(context, attention, 0);
+        attention = causal_mask(context, attention, 0);
         attention = ggml_soft_max_ext(context, attention, nullptr,
                                       1.0F / std::sqrt(static_cast<float>(head_dimension)), 0.0F);
 
PATCH

  "$cmake" -S . -B build -DCMAKE_BUILD_TYPE=Release -DGGML_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON \
    -DGGML_NATIVE=ON -DLEVO_BUILD_TESTS=OFF -DLEVO_BUILD_PARITY_TOOLS=OFF
  "$cmake" --build build -j "$(sysctl -n hw.ncpu 2>/dev/null || echo 8)" --target levo-cli levo-render
} 1>&2

[ -x "$bin/levo-cli" ] && [ -x "$bin/levo-render" ] || { echo "build finished without levo-cli/levo-render" >&2; exit 1; }
touch "$marker"
echo "$bin"
