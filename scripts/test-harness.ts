import * as fs from "fs/promises";
import * as path from "path";
import { initDatabase, type AbsFilePath, type TrackedSourceId } from "./db.ts";
import { PKB } from "./pkb.ts";
import { IndexManager, type Logger } from "./index-manager.ts";
import { MockEmbeddingModel, respondToEmbedRequest } from "./embedding/mock.ts";
import { MockLLM } from "./llm.ts";

export type TestContext = {
  tmpDir: string;
  filesDir: AbsFilePath;
  dbPath: AbsFilePath;
  mockEmbed: MockEmbeddingModel;
  mockLLM: MockLLM;
  pkb: PKB;
  manager: IndexManager;
  logger: Logger;
  logs: string[];
  trackedSourceId: TrackedSourceId;

  writeFile(filename: string, content: string): Promise<void>;
  deleteFile(filename: string): Promise<void>;
};

export async function withTestHarness(
  fn: (ctx: TestContext) => Promise<void>,
): Promise<void> {
  const testId = Math.random().toString(36).substring(2, 15);
  const tmpDir = path.join("/tmp/pkb-test", testId);
  const filesDir = path.join(tmpDir, "files") as AbsFilePath;
  const dbPath = path.join(tmpDir, "pkb.db") as AbsFilePath;

  await fs.mkdir(filesDir, { recursive: true });

  const db = initDatabase(dbPath);
  const mockEmbed = new MockEmbeddingModel();
  const mockLLM = new MockLLM();

  const logs: string[] = [];
  const logger: Logger = {
    info: (msg: string) => logs.push(`[INFO] ${msg}`),
    debug: (msg: string) => logs.push(`[DEBUG] ${msg}`),
    error: (msg: string) => logs.push(`[ERROR] ${msg}`),
  };

  const pkb = new PKB(
    { db, embeddingModel: mockEmbed, llm: mockLLM },
    { logger },
  );

  // Track the files directory by default
  const trackedSource = pkb.addTrackedSource(filesDir, "directory");

  const manager = new IndexManager(pkb, logger);

  const ctx: TestContext = {
    tmpDir,
    filesDir,
    dbPath,
    mockEmbed,
    mockLLM,
    pkb,
    manager,
    logger,
    logs,
    trackedSourceId: trackedSource.id,

    async writeFile(filename: string, content: string) {
      await fs.writeFile(path.join(filesDir, filename), content);
    },

    async deleteFile(filename: string) {
      await fs.unlink(path.join(filesDir, filename));
    },
  };

  try {
    await fn(ctx);
  } finally {
    pkb.close();
    await fs.rm(tmpDir, { recursive: true, force: true });
  }
}

export { respondToEmbedRequest };
