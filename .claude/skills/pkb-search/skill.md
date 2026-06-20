---
name: pkb-search
description: Semantic code+docs search over this repo using the pkb CLI. Use when you need broad orientation in the codebase ("where is X handled?", "how does Y work?") rather than an exact symbol or string you'd grep for.
---

# pkb — semantic search over this repo

`pkb` is a prebuilt binary at the repo root (`./pkb`) backed by a semantic
index of this repo's code and docs. Use it for orientation questions where you
don't already know the file or exact string. For exact symbols/strings, use `rg`.

```bash
./pkb search "<natural language query>"   # -k N sets result count (default 5)
./pkb stats                               # index commit, indexedAt, file/chunk counts
./pkb reindex                             # sync the index against HEAD
```

Each result is a scored snippet with its file path — treat it as a pointer and
open the file with `get_file` to read the real code. The index reflects the last
indexed commit, not your working tree; the pre-push hook reindexes
automatically, so you don't need to reindex local changes.

See the "Searching this repo" section of `context.md` for the full setup
(binary, checked-in `pkb.db`, git hooks).
