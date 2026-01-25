# Benchling Grimoire

A personal knowledge base tool with semantic search capabilities, using embeddings and vector storage.

## Project Structure

```
scripts/
├── cast.ts              # CLI entry point for sync/reindex commands
├── divine.ts            # Search result formatting utilities
├── grimoire.ts          # Core Grimoire class - indexing, search, database operations
├── inscribe.ts          # Main exports/barrel file
├── inscribe-manager.ts  # High-level manager for file watching and reindexing
├── db.ts                # SQLite database initialization and vec table setup
├── chunker.ts           # Markdown chunking logic
├── context-generator.ts # LLM-based context generation for chunks
├── create-grimoire.ts   # Factory function for creating Grimoire instances
├── llm.ts               # Bedrock Haiku LLM client for context generation
├── embedding/
│   ├── types.ts         # Embedding model interface
│   ├── bedrock-cohere.ts  # Cohere embeddings via AWS Bedrock
│   └── mock.ts          # Mock embedding model for testing
└── utils/
    └── async.ts         # Async utilities (pollUntil, Defer, delay)

spells/                  # Markdown documents to be indexed (the "spells")
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

Sync all files in `./spells` directory:
```bash
npx tsx scripts/cast.ts sync
```

Force reindex a specific file:
```bash
npx tsx scripts/cast.ts reindex <filename>
# Example: npx tsx scripts/cast.ts reindex benchling-configs.md
```

Database is always stored at `./grimoire.db` and spells are read from `./spells/`.

## Dependencies

- `@anthropic-ai/sdk` / `@anthropic-ai/bedrock-sdk` - LLM API for context generation
- `@aws-sdk/client-bedrock-runtime` - AWS Bedrock for Cohere embeddings
- `better-sqlite3` - SQLite database
- `sqlite-vec` - Vector similarity search extension for SQLite
