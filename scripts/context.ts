import type { EmbeddingModel } from "./embedding/types.ts";
import type { LLM } from "./llm.ts";
import type { GrimoireDatabase, AbsFilePath } from "./db.ts";
import { initDatabase, DEFAULT_DB_PATH, DEFAULT_SPELLS_DIR } from "./db.ts";

export type GrimoireOptions = {
  dbPath: AbsFilePath;
  spellsDir: AbsFilePath;
};

export const DEFAULT_OPTIONS: GrimoireOptions = {
  dbPath: DEFAULT_DB_PATH,
  spellsDir: DEFAULT_SPELLS_DIR,
};

export type GrimoireContext = {
  db: GrimoireDatabase;
  embeddingModel: EmbeddingModel;
  spellsDir: AbsFilePath;
  llm?: LLM;
};

export function createContext(
  options: GrimoireOptions,
  embeddingModel: EmbeddingModel,
  llm?: LLM,
): GrimoireContext {
  const db = initDatabase(options.dbPath);
  return {
    db,
    embeddingModel,
    spellsDir: options.spellsDir,
    llm,
  };
}
