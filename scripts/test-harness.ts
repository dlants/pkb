import * as fs from "fs/promises";
import * as path from "path";
import { initDatabase, type AbsFilePath } from "./db.ts";
import { Grimoire } from "./grimoire.ts";
import { InscribeManager, type Logger } from "./inscribe-manager.ts";
import { MockEmbeddingModel, respondToEmbedRequest } from "./embedding/mock.ts";
import { MockLLM } from "./llm.ts";

export type TestContext = {
  tmpDir: string;
  spellsDir: AbsFilePath;
  dbPath: AbsFilePath;
  mockEmbed: MockEmbeddingModel;
  mockLLM: MockLLM;
  grimoire: Grimoire;
  manager: InscribeManager;
  logger: Logger;
  logs: string[];

  writeSpell(filename: string, content: string): Promise<void>;
  deleteSpell(filename: string): Promise<void>;
};

export async function withTestHarness(
  fn: (ctx: TestContext) => Promise<void>,
): Promise<void> {
  const testId = Math.random().toString(36).substring(2, 15);
  const tmpDir = path.join("/tmp/grimoire-test", testId);
  const spellsDir = path.join(tmpDir, "spells") as AbsFilePath;
  const dbPath = path.join(tmpDir, "grimoire.db") as AbsFilePath;

  await fs.mkdir(spellsDir, { recursive: true });

  const db = initDatabase(dbPath);
  const mockEmbed = new MockEmbeddingModel();
  const mockLLM = new MockLLM();

  const logs: string[] = [];
  const logger: Logger = {
    info: (msg) => logs.push(`[INFO] ${msg}`),
    debug: (msg) => logs.push(`[DEBUG] ${msg}`),
    error: (msg) => logs.push(`[ERROR] ${msg}`),
  };

  const grimoire = new Grimoire(
    { db, embeddingModel: mockEmbed, spellsDir, llm: mockLLM },
    { logger },
  );

  const manager = new InscribeManager({ spellsDir }, grimoire, logger);

  const ctx: TestContext = {
    tmpDir,
    spellsDir,
    dbPath,
    mockEmbed,
    mockLLM,
    grimoire,
    manager,
    logger,
    logs,

    async writeSpell(filename: string, content: string) {
      await fs.writeFile(path.join(spellsDir, filename), content);
    },

    async deleteSpell(filename: string) {
      await fs.unlink(path.join(spellsDir, filename));
    },
  };

  try {
    await fn(ctx);
  } finally {
    grimoire.close();
    await fs.rm(tmpDir, { recursive: true, force: true });
  }
}

export { respondToEmbedRequest };
