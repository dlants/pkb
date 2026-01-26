#!/usr/bin/env npx tsx
import * as fs from "fs";
import * as path from "path";
import { PKB } from "./pkb.ts";
import { IndexManager, type Logger } from "./index-manager.ts";
import { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
import { createBedrockHaikuLLM } from "./llm.ts";
import { formatResults } from "./search.ts";
import { createContext, type PKBContext } from "./context.ts";
import type { AbsFilePath, TrackedSourceId } from "./db.ts";

const logger: Logger = {
  info: (msg: string) => console.log(msg),
  debug: (msg: string) => console.log(`[debug] ${msg}`),
  error: (msg: string) => console.error(msg),
};

function printUsage() {
  console.error("Usage:");
  console.error("  npx tsx scripts/cli.ts <dbPath> track <path>");
  console.error("    Track a file or directory for indexing");
  console.error("");
  console.error("  npx tsx scripts/cli.ts <dbPath> untrack <path>");
  console.error("    Stop tracking a file or directory");
  console.error("");
  console.error("  npx tsx scripts/cli.ts <dbPath> list");
  console.error("    List all tracked sources");
  console.error("");
  console.error("  npx tsx scripts/cli.ts <dbPath> sync");
  console.error("    Sync all tracked sources");
  console.error("");
  console.error("  npx tsx scripts/cli.ts <dbPath> reindex <file>");
  console.error("    Force reindex a specific file (absolute path)");
  console.error("");
  console.error("  npx tsx scripts/cli.ts <dbPath> search <query> [topK]");
  console.error("    Search the PKB for relevant chunks");
  console.error("");
  console.error("Examples:");
  console.error("  npx tsx scripts/cli.ts ./pkb.db track ./files");
  console.error("  npx tsx scripts/cli.ts ~/my.db track ~/docs/notes.md");
  console.error("  npx tsx scripts/cli.ts ./pkb.db list");
  console.error("  npx tsx scripts/cli.ts ./pkb.db sync");
  console.error(
    "  npx tsx scripts/cli.ts ./pkb.db reindex /home/user/docs/notes.md",
  );
  console.error(
    '  npx tsx scripts/cli.ts ./pkb.db search "how do I configure X"',
  );
}

function createPKBContext(dbPath: string): PKBContext {
  const embeddingModel = new BedrockCohereEmbedding();
  const llm = createBedrockHaikuLLM();
  const options = { dbPath: resolvePath(dbPath) };
  return createContext(options, embeddingModel, llm);
}

function createPKB(ctx: PKBContext): PKB {
  return new PKB(ctx, { logger });
}

function resolvePath(inputPath: string): AbsFilePath {
  return path.resolve(inputPath) as AbsFilePath;
}

async function trackCommand(dbPath: string, inputPath: string) {
  const ctx = createPKBContext(dbPath);
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

async function untrackCommand(dbPath: string, inputPath: string) {
  const ctx = createPKBContext(dbPath);
  const pkb = createPKB(ctx);

  try {
    const absPath = resolvePath(inputPath);
    pkb.removeTrackedSource(absPath);
    logger.info(`Untracked: ${absPath}`);
  } finally {
    pkb.close();
  }
}

function listCommand(dbPath: string) {
  const ctx = createPKBContext(dbPath);
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

async function syncCommand(dbPath: string) {
  const ctx = createPKBContext(dbPath);
  const pkb = createPKB(ctx);
  const manager = new IndexManager(pkb, logger);

  try {
    await manager.reindex();
  } finally {
    pkb.close();
  }
}

async function reindexCommand(dbPath: string, inputPath: string) {
  const ctx = createPKBContext(dbPath);
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

async function searchCommand(dbPath: string, query: string, topK: number = 10) {
  const ctx = createPKBContext(dbPath);
  const pkb = createPKB(ctx);

  try {
    const results = await pkb.search(query, topK);
    console.log(formatResults(results));
  } finally {
    pkb.close();
  }
}

async function main() {
  const dbPath = process.argv[2];
  const command = process.argv[3];

  if (!dbPath || !command) {
    printUsage();
    process.exit(1);
  }

  switch (command) {
    case "track": {
      const inputPath = process.argv[4];
      if (!inputPath) {
        console.error("Error: track command requires a path");
        printUsage();
        process.exit(1);
      }
      await trackCommand(dbPath, inputPath);
      break;
    }

    case "untrack": {
      const inputPath = process.argv[4];
      if (!inputPath) {
        console.error("Error: untrack command requires a path");
        printUsage();
        process.exit(1);
      }
      await untrackCommand(dbPath, inputPath);
      break;
    }

    case "list":
      listCommand(dbPath);
      break;

    case "sync":
      await syncCommand(dbPath);
      break;

    case "reindex": {
      const inputPath = process.argv[4];
      if (!inputPath) {
        console.error("Error: reindex command requires a file path");
        printUsage();
        process.exit(1);
      }
      await reindexCommand(dbPath, inputPath);
      break;
    }

    case "search": {
      const query = process.argv[4];
      if (!query) {
        console.error("Error: search command requires a query");
        printUsage();
        process.exit(1);
      }
      const topK = process.argv[5] ? parseInt(process.argv[5], 10) : 10;
      await searchCommand(dbPath, query, topK);
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
