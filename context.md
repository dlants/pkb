# PKB (Personal Knowledge Base)

A personal knowledge base tool with semantic search capabilities, using embeddings and vector storage.

## Project Structure

```
scripts/
├── cli.ts               # CLI entry point for sync/reindex/search commands
├── search.ts            # Search result formatting utilities
├── pkb.ts               # Core PKB class - indexing, search, database operations
├── index.ts             # Main exports/barrel file
├── index-manager.ts     # High-level manager for file watching and reindexing
├── db.ts                # SQLite database initialization and vec table setup
├── chunker.ts           # Markdown chunking logic
├── context-generator.ts # LLM-based context generation for chunks
├── create-pkb.ts        # Factory function for creating PKB instances
├── llm.ts               # Bedrock Haiku LLM client for context generation
├── embedding/
│   ├── types.ts         # Embedding model interface
│   ├── bedrock-cohere.ts  # Cohere embeddings via AWS Bedrock
│   └── mock.ts          # Mock embedding model for testing
└── utils/
    └── async.ts         # Async utilities (pollUntil, Defer, delay)

files/                   # Markdown documents to be indexed
```

## Commands

### Type Checking
```bash
npx tsc --noEmit
```

### Run Tests
```bash
npx vitest run
```

### Watch Tests
```bash
npx vitest
```

### CLI Usage

Track a file or directory for indexing:
```bash
npx tsx scripts/cli.ts track <path>
# Example: npx tsx scripts/cli.ts track ./files
# Example: npx tsx scripts/cli.ts track ~/docs/notes.md
```

Stop tracking a file or directory:
```bash
npx tsx scripts/cli.ts untrack <path>
# Example: npx tsx scripts/cli.ts untrack ./files
```

List all tracked sources:
```bash
npx tsx scripts/cli.ts list
```

Sync all tracked sources:
```bash
npx tsx scripts/cli.ts sync
```

Force reindex a specific file (must be within a tracked source):
```bash
npx tsx scripts/cli.ts reindex <file>
# Example: npx tsx scripts/cli.ts reindex /home/user/docs/notes.md
```

Search the PKB:
```bash
npx tsx scripts/cli.ts search "<query>" [topK]
# Example: npx tsx scripts/cli.ts search "how do I configure X" 5
```

Database is stored at `./pkb.db`. Tracked sources can be from any location.

## Dependencies

- `@anthropic-ai/sdk` / `@anthropic-ai/bedrock-sdk` - LLM API for context generation
- `@aws-sdk/client-bedrock-runtime` - AWS Bedrock for Cohere embeddings
- `better-sqlite3` - SQLite database
- `sqlite-vec` - Vector similarity search extension for SQLite
