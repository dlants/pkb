#!/usr/bin/env npx tsx
import * as fs from "fs";
import * as path from "path";
import { PKB } from "./pkb.ts";
import { IndexManager, type Logger } from "./index-manager.ts";
import { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
import { createBedrockHaikuLLM } from "./llm.ts";
import { formatResults } from "./search.ts";
import { createContext, DEFAULT_OPTIONS, type PKBContext } from "./context.ts";
import type { AbsFilePath, TrackedSourceId } from "./db.ts";

const logger: Logger = {
  info: (msg: string) => console.log(msg),
  debug: (msg: string) => console.log(`[debug] ${msg}`),
  error: (msg: string) => console.error(msg),
};

function printUsage() {
  console.error("Usage:");
  console.error("  npx tsx scripts/cli.ts track <path>");
  console.error("    Track a file or directory for indexing");
  console.error("");
  console.error("  npx tsx scripts/cli.ts untrack <path>");
  console.error("    Stop tracking a file or directory");
  console.error("");
  console.error("  npx tsx scripts/cli.ts list");
  console.error("    List all tracked sources");
  console.error("");
  console.error("  npx tsx scripts/cli.ts sync");
  console.error("    Sync all tracked sources");
  console.error("");
  console.error("  npx tsx scripts/cli.ts reindex <file>");
  console.error("    Force reindex a specific file (absolute path)");
  console.error("");
  console.error("  npx tsx scripts/cli.ts search <query> [topK]");
  console.error("    Search the PKB for relevant chunks");
  console.error("");
  console.error("Examples:");
  console.error("  npx tsx scripts/cli.ts track ./files");
  console.error("  npx tsx scripts/cli.ts track ~/docs/notes.md");
  console.error("  npx tsx scripts/cli.ts list");
  console.error("  npx tsx scripts/cli.ts sync");
  console.error("  npx tsx scripts/cli.ts reindex /home/user/docs/notes.md");
  console.error('  npx tsx scripts/cli.ts search "how do I configure X"');
}

function createPKBContext(): PKBContext {
  const embeddingModel = new BedrockCohereEmbedding();
  const llm = createBedrockHaikuLLM();
  return createContext(DEFAULT_OPTIONS, embeddingModel, llm);
}

function createPKB(ctx: PKBContext): PKB {
  return new PKB(ctx, { logger });
}

function resolvePath(inputPath: string): AbsFilePath {
  return path.resolve(inputPath) as AbsFilePath;
}

async function trackCommand(inputPath: string) {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);

  try {
    const absPath = resolvePath(inputPath);

    if (!fs.existsSync(absPath)) {
      console.error(`Error: Path does not exist: ${absPath}`);
      process.exit(1);
    }

    const stat = fs.statSync(absPath);
    const type = stat.isDirectory() ? "directory" : "file";

    const source = pkb.addTrackedSource(absPath, type);
    logger.info(`Tracking ${type}: ${absPath}`);

    // Immediately index the tracked source
    const manager = new IndexManager(pkb, logger);
    await manager.reindex();
    logger.info(`Done. Tracked source ID: ${source.id}`);
  } finally {
    pkb.close();
  }
}

async function untrackCommand(inputPath: string) {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);

  try {
    const absPath = resolvePath(inputPath);
    pkb.removeTrackedSource(absPath);
    logger.info(`Untracked: ${absPath}`);
  } finally {
    pkb.close();
  }
}

function listCommand() {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);

  try {
    const sources = pkb.getTrackedSources();

    if (sources.length === 0) {
      console.log("No tracked sources.");
      return;
    }

    console.log("Tracked sources:");
    for (const source of sources) {
      const date = new Date(source.createdAt).toISOString();
      console.log(`  [${source.type}] ${source.path} (added ${date})`);
    }
  } finally {
    pkb.close();
  }
}

async function syncCommand() {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);
  const manager = new IndexManager(pkb, logger);

  try {
    await manager.reindex();
  } finally {
    pkb.close();
  }
}

async function reindexCommand(inputPath: string) {
  const ctx = createPKBContext();
  const pkb = createPKB(ctx);

  try {
    const absPath = resolvePath(inputPath);

    // Clean up any orphan vec entries from previous failed indexing attempts
    const orphansDeleted = pkb.cleanupOrphanVecEntries();
    if (orphansDeleted > 0) {
      logger.info(`Cleaned up ${orphansDeleted} orphan vector entries`);
    }

    // Find the tracked source for this file
    const sources = pkb.getTrackedSources();
    let trackedSourceId: TrackedSourceId | undefined;

    for (const source of sources) {
      if (source.type === "file" && source.path === absPath) {
        trackedSourceId = source.id;
        break;
      }
      if (
        source.type === "directory" &&
        absPath.startsWith(source.path + "/")
      ) {
        trackedSourceId = source.id;
        break;
      }
    }

    if (!trackedSourceId) {
      console.error(`Error: File is not in a tracked source: ${absPath}`);
      console.error("Use 'track' to add a tracked source first.");
      process.exit(1);
    }

    // Find all file records matching this path (including those with empty hash)
    const fileIds = pkb.getFileIdsByFilename(
      absPath as unknown as import("./pkb.ts").PKBFile,
    );

    if (fileIds.length > 0) {
      logger.info(
        `Deleting ${fileIds.length} existing record(s) for ${absPath}...`,
      );
      for (const fileId of fileIds) {
        pkb.deleteFile(fileId);
      }
    }

    logger.info(`Reindexing ${absPath}...`);
    await pkb.indexFile(absPath, trackedSourceId);
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
    case "track": {
      const inputPath = process.argv[3];
      if (!inputPath) {
        console.error("Error: track command requires a path");
        printUsage();
        process.exit(1);
      }
      await trackCommand(inputPath);
      break;
    }

    case "untrack": {
      const inputPath = process.argv[3];
      if (!inputPath) {
        console.error("Error: untrack command requires a path");
        printUsage();
        process.exit(1);
      }
      await untrackCommand(inputPath);
      break;
    }

    case "list":
      listCommand();
      break;

    case "sync":
      await syncCommand();
      break;

    case "reindex": {
      const inputPath = process.argv[3];
      if (!inputPath) {
        console.error("Error: reindex command requires a file path");
        printUsage();
        process.exit(1);
      }
      await reindexCommand(inputPath);
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
