# PKB (Personal Knowledge Base)

A CLI for managing a local semantic search-based personal knowledge base. Index your markdown documents and search them using natural language queries.

- **Standalone**: Manage and search your own notes, documentation, and knowledge base via the cli.
- **Agent Skill**: Expose your knowledge base to AI agents like Claude Code by creating a skill.
- **Single Db file**: Commit the db to git to turn your repo's markdown docs into a searchable knowledge base for your whole team—just run sync (maybe in CI/CD) to keep everything up to date.

Currently only Markdown files (`.md`) are supported. Uses brute-force search to get the nearest neighbors (no approximate nearest neighbors), so this is best suited for small to medium document collections (not full codebases).

## Quick Start

### Requirements

- Node.js 18+
- AWS credentials configured (access to Bedrock for cohere embed v4 model and anthropic haiku 4.5)
- `us-east-1` or `eu-west-1` region access for Cohere Embed v4
- SQLite3 with [sqlite-vec](https://github.com/asg017/sqlite-vec) extension

```bash
# Install dependencies
npm install

# Track a directory of markdown files
npx tsx scripts/cli.ts ~/pkb.db track ~/docs

# Index all tracked sources
npx tsx scripts/cli.ts ~/pkb.db sync

# Search your knowledge base
npx tsx scripts/cli.ts ~/pkb.db search "how to configure X"
```

## Exposing to Claude Code

You can expose your PKB to Claude Code (or similar agents) by creating a skill:

1. Create a skill directory:

   ```bash
   mkdir -p ~/.claude/skills/my-knowledge
   ```

2. Clone this repo into the skill directory:

   ```bash
   git clone <repo-url> ~/.claude/skills/my-knowledge/pkb
   cd ~/.claude/skills/my-knowledge/pkb
   npm install
   ```

3. Create `~/.claude/skills/my-knowledge/skill.md`:

   ```markdown
   ---
   name: my-knowledge
   description: My personal knowledge base containing notes on <topics>. Search here for <what kind of information>.
   ---

   ## Searching

   \`\`\`bash
   npx tsx ~/.claude/skills/my-knowledge/pkb/scripts/cli.ts ~/.pkb/knowledge.db search "<query>" [topK]
   \`\`\`
   ...
   ```

4. Track your files and sync:

   ```bash
   npx tsx ~/.claude/skills/my-knowledge/pkb/scripts/cli.ts ~/.pkb/knowledge.db track ~/notes
   npx tsx ~/.claude/skills/my-knowledge/pkb/scripts/cli.ts ~/.pkb/knowledge.db sync
   ```

5. (Optional) Run sync in watch mode to automatically reindex tracked sources on file changes:
   ```bash
   npx tsx ~/.claude/skills/my-knowledge/pkb/scripts/cli.ts ~/.pkb/knowledge.db sync --watch
   ```

## How It Works

### Chunking

Markdown documents are split into chunks for indexing:

1. **Hard splits** on headings (h1-h6) - each section becomes a separate unit
2. **Soft splits** on paragraphs and code blocks when sections exceed ~2000 characters (~500 tokens)
3. **Sentence-level splits** for very long paragraphs
4. **Character-level splits** with overlap as a last resort

Each chunk preserves its heading hierarchy context (e.g., `# Guide > ## Configuration > ### AWS`).

### Contextual Retrieval

PKB implements [Contextual Retrieval](https://www.anthropic.com/engineering/contextual-retrieval) to improve search accuracy. Before embedding each chunk, an LLM generates additional context that situates the chunk within the full document.

For example, a chunk containing:

> Set the ACL to private to restrict access.

Gets augmented with context like:

> This chunk describes AWS S3 bucket access control configuration. ACL refers to Access Control List.

This context is prepended to the chunk before embedding, helping the vector search understand ambiguous references and acronyms.

### Models

**Embeddings**: Cohere Embed v4 via AWS Bedrock
**Context Generation**: Claude 4.5 Haiku via AWS Bedrock

Other models are easily implemented by extending the existing interfaces.

## CLI Reference

```
npx tsx scripts/cli.ts --help
```
