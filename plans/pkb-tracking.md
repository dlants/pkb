# PKB Tracking Plan

## Context

**Objective:** Instead of providing a pkb directory and indexing all `.md` files within it, maintain explicit tracking of files/directories in sqlite. This allows tracking files from multiple locations and gives explicit control over what's indexed.

**Relevant files:**

- `scripts/db.ts`: Database initialization and schema. Contains `initDatabase()`, `ensureVecTable()`, and table definitions for `files` and `chunks`. Has `DEFAULT_DB_PATH` and `DEFAULT_FILES_DIR` constants.
- `scripts/pkb.ts`: Core PKB class. Has `indexFile()`, `deleteFile()`, `search()`, `getIndexedFiles()`. Takes `ctx` with `db`, `embeddingModel`, `filesDir`, and optional `llm`.
- `scripts/index-manager.ts`: Manages periodic reindexing. `scanForChanges()` scans `filesDir` directory for `.md` files, `reindex()` processes the queue.
- `scripts/cli.ts`: CLI entry point. Currently has `sync`, `reindex <file>`, and `search <query>` commands. Uses `createContext()` from `context.ts`.

**Key types:**

```typescript
// Current files table schema (in db.ts)
// id, filename, model_name, embedding_version, mtime_ms, hash

// IndexedFileInfo (in pkb.ts)
type IndexedFileInfo = {
  id: FileId;
  filename: PKBFile;
  mtimeMs: MtimeMs;
  hash: FileHash;
};

// FileOperation (in pkb.ts)
type FileOperation =
  | { type: "index"; filename: PKBFile }
  | { type: "delete"; filename: PKBFile; fileId: FileId };

// PKBContext (in context.ts)
type PKBContext = {
  db: GrimoireDatabase;
  embeddingModel: EmbeddingModel;
  filesDir: AbsFilePath;
  llm?: LLM;
};
```

**New types to add:**

```typescript
type TrackedSourceId = number & { __tracked_source_id: true };

type TrackedSource = {
  id: TrackedSourceId;
  path: AbsFilePath; // absolute path to file or directory
  type: "file" | "directory";
  createdAt: number; // timestamp
};
```

**Schema changes:**

```sql
-- New table
CREATE TABLE IF NOT EXISTS tracked_sources (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

-- Add to files table
ALTER TABLE files ADD COLUMN tracked_source_id INTEGER REFERENCES tracked_sources(id);
```

Every file in the `files` table references the `tracked_source` that caused it to be indexed. This makes cleanup easy when untracking.

**CLI changes:**

```
npx tsx scripts/cli.ts track <path>        # Track a file or directory
npx tsx scripts/cli.ts untrack <path>      # Stop tracking
npx tsx scripts/cli.ts list                # Show all tracked sources
npx tsx scripts/cli.ts sync                # Sync all tracked sources
npx tsx scripts/cli.ts reindex <file>      # Force reindex a specific file
npx tsx scripts/cli.ts search <query>      # Search the PKB
```

## Implementation

- [x] Update schema in `scripts/db.ts`
  - [x] Add `tracked_sources` table creation
  - [x] Add `tracked_source_id` column to `files` table
  - [x] Run type check

- [x] Add tracked source management methods to `PKB` in `scripts/pkb.ts`
  - [x] `addTrackedSource(path: AbsFilePath, type: "file" | "directory"): TrackedSource`
  - [x] `removeTrackedSource(path: AbsFilePath): void` - also deletes all files with matching `tracked_source_id`
  - [x] `getTrackedSources(): TrackedSource[]`
  - [x] Run type check

- [x] Update `indexFile()` to accept `trackedSourceId` parameter
  - [x] Store `tracked_source_id` when inserting into `files` table
  - [x] Change `filename` to store absolute path instead of relative path
  - [x] Run type check and fix callers

- [x] Update `IndexManager.scanForChanges()` in `scripts/index-manager.ts` to iterate tracked sources
  - [x] Remove `filesDir` from constructor context (no longer needed)
  - [x] For each tracked source:
    - If directory: recursively find `.md` files
    - If file: check that specific file
  - [x] Compare against indexed files with matching `tracked_source_id`
  - [x] If tracked path is missing: queue delete operations for its files (keep tracking record)
  - [x] Run type check

- [x] Update CLI in `scripts/cli.ts`
  - [x] Add `track` command: determine file/dir, call `addTrackedSource()`, trigger immediate index
  - [x] Add `untrack` command: call `removeTrackedSource()`
  - [x] Add `list` command: call `getTrackedSources()` and display
  - [x] Update `sync` command to work with tracked sources (no path arg needed)
  - [x] Update `reindex` command to accept absolute path
  - [x] Run type check

- [x] Update tests
  - [x] Update existing tests in `scripts/pkb.spec.ts` and `scripts/index-manager.spec.ts`
  - [x] Add tests for `addTrackedSource`, `removeTrackedSource`, `getTrackedSources`
  - [x] Add tests for `sync` with tracked directories (covered by existing tests with tracked filesDir)
  - [x] Add tests for missing tracked paths behavior (covered by IndexManager.scanForChanges)
  - [x] Run tests and iterate until passing
