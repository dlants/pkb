#!/usr/bin/env npx tsx
import { Grimoire, type Spell } from "./grimoire.ts";
import { InscribeManager, type Logger } from "./inscribe-manager.ts";
import { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
import { createBedrockHaikuLLM } from "./llm.ts";
import { formatResults } from "./divine.ts";
import { createContext, DEFAULT_OPTIONS, type GrimoireContext } from "./context.ts";

const logger: Logger = {
  info: (msg: string) => console.log(msg),
  debug: (msg: string) => console.log(`[debug] ${msg}`),
  error: (msg: string) => console.error(msg),
};

function printUsage() {
  console.error("Usage:");
  console.error("  npx tsx scripts/cast.ts sync");
  console.error("    Sync all files in ./spells directory");
  console.error("");
  console.error("  npx tsx scripts/cast.ts inscribe <spell>");
  console.error(
    "    Force reindex a specific spell (deletes and re-embeds. Path is relative to the /spells dir)",
  );
  console.error("");
  console.error("  npx tsx scripts/cast.ts divine <query> [topK]");
  console.error("    Search the grimoire for relevant chunks");
  console.error("");
  console.error("Examples:");
  console.error("  npx tsx scripts/cast.ts sync");
  console.error("  npx tsx scripts/cast.ts inscribe benchling-configs.md");
  console.error('  npx tsx scripts/cast.ts divine "how do I configure X"');
  console.error('  npx tsx scripts/cast.ts divine "how do I configure X" 5');
}

function createGrimoireContext(): GrimoireContext {
  const embeddingModel = new BedrockCohereEmbedding();
  const llm = createBedrockHaikuLLM();
  return createContext(DEFAULT_OPTIONS, embeddingModel, llm);
}

function createGrimoire(ctx: GrimoireContext): Grimoire {
  return new Grimoire(ctx, { logger });
}

async function syncCommand() {
  const ctx = createGrimoireContext();
  const grimoire = createGrimoire(ctx);
  const manager = new InscribeManager({ spellsDir: ctx.spellsDir }, grimoire, logger);

  try {
    await manager.reindex();
  } finally {
    grimoire.close();
  }
}

async function inscribeCommand(filename: Spell) {
  const ctx = createGrimoireContext();
  const grimoire = createGrimoire(ctx);

  try {
    // Clean up any orphan vec entries from previous failed indexing attempts
    const orphansDeleted = grimoire.cleanupOrphanVecEntries();
    if (orphansDeleted > 0) {
      logger.info(`Cleaned up ${orphansDeleted} orphan vector entries`);
    }

    // Find all file records matching this filename (including those with empty hash)
    const fileIds = grimoire.getFileIdsByFilename(filename);

    if (fileIds.length > 0) {
      logger.info(
        `Deleting ${fileIds.length} existing record(s) for ${filename}...`,
      );
      for (const fileId of fileIds) {
        grimoire.deleteFile(fileId);
      }
    }

    logger.info(`Reindexing ${filename}...`);
    await grimoire.indexFile(filename);
    logger.info("Done.");
  } finally {
    grimoire.close();
  }
}

async function divineCommand(query: string, topK: number = 10) {
  const ctx = createGrimoireContext();
  const grimoire = createGrimoire(ctx);

  try {
    const results = await grimoire.search(query, topK);
    console.log(formatResults(results));
  } finally {
    grimoire.close();
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

    case "inscribe": {
      const filename = process.argv[3];
      if (!filename) {
        console.error("Error: inscribe command requires a filename");
        printUsage();
        process.exit(1);
      }
      await inscribeCommand(filename as Spell);
      break;
    }

    case "divine": {
      const query = process.argv[3];
      if (!query) {
        console.error("Error: divine command requires a query");
        printUsage();
        process.exit(1);
      }
      const topK = process.argv[4] ? parseInt(process.argv[4], 10) : 10;
      await divineCommand(query, topK);
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
