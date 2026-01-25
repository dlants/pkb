import type { EmbeddingModel } from "./embedding/types.ts";
import type { LLM } from "./llm.ts";
import type { GrimoireDatabase, AbsFilePath } from "./db.ts";
import { initDatabase, DEFAULT_DB_PATH, DEFAULT_FILES_DIR } from "./db.ts";

export type PKBOptions = {
  dbPath: AbsFilePath;
  filesDir: AbsFilePath;
};

export const DEFAULT_OPTIONS: PKBOptions = {
  dbPath: DEFAULT_DB_PATH,
  filesDir: DEFAULT_FILES_DIR,
};

export type PKBContext = {
  db: GrimoireDatabase;
  embeddingModel: EmbeddingModel;
  filesDir: AbsFilePath;
  llm?: LLM;
};

export function createContext(
  options: PKBOptions,
  embeddingModel: EmbeddingModel,
  llm?: LLM,
): PKBContext {
  const db = initDatabase(options.dbPath);
  return {
    db,
    embeddingModel,
    filesDir: options.filesDir,
    llm,
  };
}
