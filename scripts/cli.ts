#!/usr/bin/env npx tsx
import { PKB, type PKBFile } from "./pkb.ts";
import { IndexManager, type Logger } from "./index-manager.ts";
import { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
import { createBedrockHaikuLLM } from "./llm.ts";
import { formatResults } from "./search.ts";
import { createContext, DEFAULT_OPTIONS, type PKBContext } from "./context.ts";

const logger: Logger = {
  info: (msg: string) => console.log(msg),
  debug: (msg: string) => console.log(`[debug] ${msg}`),
  error: (msg: string) => console.error(msg),
};

function printUsage() {
  console.error("Usage:");
  console.error("  npx tsx scripts/cli.ts sync");
  console.error("    Sync all files in ./files directory");
  console.error("");
  console.error("  npx tsx scripts/cli.ts reindex <file>");
  console.error(
    "    Force reindex a specific file (deletes and re-embeds. Path is relative to the /files dir)",
  );
  console.error("");
  console.error("  npx tsx scripts/cli.ts search <query> [topK]");
  console.error("    Search the PKB for relevant chunks");
  console.error("");
  console.error("Examples:");
  console.error("  npx tsx scripts/cli.ts sync");
  console.error("  npx tsx scripts/cli.ts reindex benchling-configs.md");
  console.error('  npx tsx scripts/cli.ts search "how do I configure X"');
  console.error('  npx tsx scripts/cli.ts search "how do I configure X" 5');
}

function createPKBContext(): PKBContext {
  const embeddingModel = new BedrockCohereEmbedding();
  const llm = createBedrockHaikuLLM();
  return createContext(DEFAULT_OPTIONS, embeddingModel, llm);
}

function createPKB(ctx: PKBContext): PKB {
  return new PKB(ctx, { logger });
}

async function syncCommand() {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);
  const manager = new IndexManager({ filesDir: ctx.filesDir }, pkb, logger);

  try {
    await manager.reindex();
  } finally {
    pkb.close();
  }
}

async function reindexCommand(filename: PKBFile) {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);

  try {
    // Clean up any orphan vec entries from previous failed indexing attempts
    const orphansDeleted = pkb.cleanupOrphanVecEntries();
    if (orphansDeleted > 0) {
      logger.info(`Cleaned up ${orphansDeleted} orphan vector entries`);
    }

    // Find all file records matching this filename (including those with empty hash)
    const fileIds = pkb.getFileIdsByFilename(filename);

    if (fileIds.length > 0) {
      logger.info(
        `Deleting ${fileIds.length} existing record(s) for ${filename}...`,
      );
      for (const fileId of fileIds) {
        pkb.deleteFile(fileId);
      }
    }

    logger.info(`Reindexing ${filename}...`);
    await pkb.indexFile(filename);
    logger.info("Done.");
  } finally {
    pkb.close();
  }
}

async function searchCommand(query: string, topK: number = 10) {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);

  try {
    const results = await pkb.search(query, topK);
    console.log(formatResults(results));
  } finally {
    pkb.close();
  }
}

async function main() {
  const command = process.argv[2];

  if (!command) {
    printUsage();
    process.exit(1);
  }

  switch (command) {
    case "sync":
      await syncCommand();
      break;

    case "reindex": {
      const filename = process.argv[3];
      if (!filename) {
        console.error("Error: reindex command requires a filename");
        printUsage();
        process.exit(1);
      }
      await reindexCommand(filename as PKBFile);
      break;
    }

    case "search": {
      const query = process.argv[3];
      if (!query) {
        console.error("Error: search command requires a query");
        printUsage();
        process.exit(1);
      }
      const topK = process.argv[4] ? parseInt(process.argv[4], 10) : 10;
      await searchCommand(query, topK);
      break;
    }

    default:
      console.error(`Unknown command: ${command}`);
      printUsage();
      process.exit(1);
  }
}

main().catch((error) => {
  console.error("Command failed:", error);
  process.exit(1);
});
