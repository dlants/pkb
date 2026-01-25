import Database from "better-sqlite3";
import * as sqliteVec from "sqlite-vec";
import * as path from "path";
import type {
  EmbeddingModelName,
  EmbeddingVersion,
} from "./embedding/types.ts";

export type GrimoireDatabase = ReturnType<typeof Database>;

export type AbsFilePath = string & { __abs_file_path: true };
export type VecTableName = string & { __vec_table_name: true };

const PROJECT_ROOT = path.resolve(import.meta.dirname, "..");
export const DEFAULT_DB_PATH = path.join(PROJECT_ROOT, "pkb.db") as AbsFilePath;
export const DEFAULT_FILES_DIR = path.join(PROJECT_ROOT, "files") as AbsFilePath;

export function initDatabase(dbPath: AbsFilePath): GrimoireDatabase {
  const db = new Database(dbPath);

  sqliteVec.load(db);

  db.exec(`
    CREATE TABLE IF NOT EXISTS files (
      id INTEGER PRIMARY KEY,
      filename TEXT NOT NULL,
      model_name TEXT NOT NULL,
      embedding_version INTEGER NOT NULL,
      mtime_ms INTEGER NOT NULL,
      hash TEXT NOT NULL,
      UNIQUE(filename, model_name, embedding_version)
    );

    CREATE TABLE IF NOT EXISTS chunks (
      id INTEGER PRIMARY KEY,
      file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
      text TEXT NOT NULL,
      contextualized_text TEXT NOT NULL,
      start_line INTEGER NOT NULL,
      start_col INTEGER NOT NULL,
      end_line INTEGER NOT NULL,
      end_col INTEGER NOT NULL
    );
  `);

  db.pragma("foreign_keys = ON");

  return db;
}

export function ensureVecTable(
  db: GrimoireDatabase,
  modelName: EmbeddingModelName,
  embeddingVersion: EmbeddingVersion,
  dimensions: number,
): void {
  const tableName = getVecTableName(modelName, embeddingVersion);
  db.exec(
    `CREATE VIRTUAL TABLE IF NOT EXISTS ${tableName} USING vec0(chunk_id INTEGER PRIMARY KEY, embedding float[${dimensions}] distance_metric=cosine)`,
  );
}

export function getVecTableName(
  modelName: EmbeddingModelName,
  embeddingVersion: EmbeddingVersion,
): VecTableName {
  const sanitized = modelName.replace(/[^a-zA-Z0-9_]/g, "_");
  return `vec_${sanitized}_v${embeddingVersion}` as VecTableName;
}
