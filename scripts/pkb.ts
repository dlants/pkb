import * as fs from "fs";
import * as path from "path";
import * as crypto from "crypto";
import {
  MAGENTA_EMBEDDING_VERSION,
  type ChunkData,
  type EmbeddingModel,
} from "./embedding/types.ts";
import { chunkMarkdown } from "./chunker.ts";
import { generateContext } from "./context-generator.ts";
import {
  ensureVecTable,
  getVecTableName,
  type GrimoireDatabase,
  type AbsFilePath,
} from "./db.ts";
import type { Logger } from "./index-manager.ts";
import type { LLM } from "./llm.ts";

// Branded types
export type FileId = number & { __file_id: true };
export type ChunkId = number & { __chunk_id: true };
export type PKBFile = string & { __pkb_file: true };
export type FileHash = string & { __file_hash: true };
export type MtimeMs = number & { __mtime_ms: true };
export type Distance = number & { __distance: true };
export type Score = number & { __score: true };

export type SearchResult = {
  file: PKBFile;
  chunk: ChunkData;
  score: Score;
};

export type IndexLogEntry = {
  file: PKBFile;
  chunkCount: number;
  timestamp: Date;
};

export type PKBStats = {
  totalFiles: number;
  totalChunks: number;
  recentFiles: IndexLogEntry[];
};

export type FileOperation =
  | { type: "index"; filename: PKBFile }
  | { type: "delete"; filename: PKBFile; fileId: FileId };

const MAX_INDEX_LOG_ENTRIES = 20;

export function computeFileHash(filePath: AbsFilePath): FileHash {
  const content = fs.readFileSync(filePath);
  return crypto.createHash("md5").update(content).digest("hex") as FileHash;
}

export type IndexedFileInfo = {
  id: FileId;
  filename: PKBFile;
  mtimeMs: MtimeMs;
  hash: FileHash;
};

export class PKB {
  public indexLog: IndexLogEntry[] = [];
  private db: GrimoireDatabase;
  private vecTableInitialized = false;
  private logger?: Logger | undefined;

  constructor(
    private ctx: {
      db: GrimoireDatabase;
      embeddingModel: EmbeddingModel;
      filesDir: AbsFilePath;
      llm?: LLM;
    },
    options?: { logger?: Logger },
  ) {
    this.db = ctx.db;
    this.logger = options?.logger;
  }

  close(): void {
    this.db.close();
  }

  private ensureVecTableInitialized(): void {
    if (this.vecTableInitialized) return;

    ensureVecTable(
      this.db,
      this.ctx.embeddingModel.modelName,
      MAGENTA_EMBEDDING_VERSION,
      this.ctx.embeddingModel.dimensions,
    );
    this.vecTableInitialized = true;
  }

  getIndexedFiles(): IndexedFileInfo[] {
    this.ensureVecTableInitialized();

    // Filter out files with empty hash (incomplete indexing)
    const rows = this.db
      .prepare<
        [string, number],
        { id: number; filename: string; mtime_ms: number; hash: string }
      >("SELECT id, filename, mtime_ms, hash FROM files WHERE model_name = ? AND embedding_version = ? AND hash != ''")
      .all(this.ctx.embeddingModel.modelName, MAGENTA_EMBEDDING_VERSION);

    return rows.map((row) => ({
      id: row.id as FileId,
      filename: row.filename as PKBFile,
      mtimeMs: row.mtime_ms as MtimeMs,
      hash: row.hash as FileHash,
    }));
  }

  getFileIdsByFilename(filename: PKBFile): FileId[] {
    this.ensureVecTableInitialized();

    const rows = this.db
      .prepare<
        [string, string, number],
        { id: number }
      >("SELECT id FROM files WHERE filename = ? AND model_name = ? AND embedding_version = ?")
      .all(
        filename,
        this.ctx.embeddingModel.modelName,
        MAGENTA_EMBEDDING_VERSION,
      );

    return rows.map((row) => row.id as FileId);
  }

  updateFileMtime(fileId: FileId, mtimeMs: MtimeMs): void {
    this.db
      .prepare<[number, number]>("UPDATE files SET mtime_ms = ? WHERE id = ?")
      .run(mtimeMs, fileId);
  }

  deleteFile(fileId: FileId): void {
    this.ensureVecTableInitialized();

    const vecTableName = getVecTableName(
      this.ctx.embeddingModel.modelName,
      MAGENTA_EMBEDDING_VERSION,
    );

    this.db
      .prepare(
        `DELETE FROM ${vecTableName} WHERE chunk_id IN (SELECT id FROM chunks WHERE file_id = ?)`,
      )
      .run(fileId);
    this.db.prepare("DELETE FROM chunks WHERE file_id = ?").run(fileId);
    this.db.prepare("DELETE FROM files WHERE id = ?").run(fileId);
  }

  cleanupOrphanVecEntries(): number {
    this.ensureVecTableInitialized();

    const vecTableName = getVecTableName(
      this.ctx.embeddingModel.modelName,
      MAGENTA_EMBEDDING_VERSION,
    );

    const result = this.db
      .prepare(
        `DELETE FROM ${vecTableName} WHERE chunk_id NOT IN (SELECT id FROM chunks)`,
      )
      .run();

    return result.changes;
  }

  async indexFile(mdFile: PKBFile): Promise<void> {
    this.ensureVecTableInitialized();

    const mdPath = path.join(this.ctx.filesDir, mdFile) as AbsFilePath;

    if (!fs.existsSync(mdPath)) {
      return;
    }

    const stat = fs.statSync(mdPath);
    const currentMtime = stat.mtimeMs as MtimeMs;
    const currentHash = computeFileHash(mdPath);

    const getFileRecord = this.db.prepare<
      [string, string, number],
      { id: number; mtime_ms: number; hash: string }
    >(
      "SELECT id, mtime_ms, hash FROM files WHERE filename = ? AND model_name = ? AND embedding_version = ?",
    );

    const existingFile = getFileRecord.get(
      mdFile,
      this.ctx.embeddingModel.modelName,
      MAGENTA_EMBEDDING_VERSION,
    );

    let fileId: FileId;
    if (existingFile) {
      fileId = existingFile.id as FileId;
    } else {
      // Create file record with empty hash to mark as "indexing in progress"
      const result = this.db
        .prepare<
          [string, string, number, number, string]
        >("INSERT INTO files (filename, model_name, embedding_version, mtime_ms, hash) VALUES (?, ?, ?, ?, ?)")
        .run(
          mdFile,
          this.ctx.embeddingModel.modelName,
          MAGENTA_EMBEDDING_VERSION,
          0,
          "",
        );
      fileId = Number(result.lastInsertRowid) as FileId;
    }

    await this.embedFile(mdPath, mdFile, currentMtime, currentHash, fileId);
  }

  private async embedFile(
    mdPath: AbsFilePath,
    mdFile: PKBFile,
    currentMtime: MtimeMs,
    currentHash: FileHash,
    fileId: FileId,
  ): Promise<void> {
    const content = fs.readFileSync(mdPath, "utf-8");
    const chunks = chunkMarkdown(content);

    const vecTableName = getVecTableName(
      this.ctx.embeddingModel.modelName,
      MAGENTA_EMBEDDING_VERSION,
    );

    // Get existing chunks for this file
    const existingChunks = this.db
      .prepare<
        [number],
        { id: number; text: string }
      >("SELECT id, text FROM chunks WHERE file_id = ?")
      .all(fileId);

    // Build a map of existing chunk text -> chunk id for quick lookup
    const existingChunkMap = new Map<string, number>();
    for (const chunk of existingChunks) {
      existingChunkMap.set(chunk.text, chunk.id);
    }

    // Track which existing chunks are still present in the new file
    const newChunkTexts = new Set(chunks.map((c) => c.text));

    // Delete chunks that no longer exist in the file
    const chunksToDelete = existingChunks.filter(
      (c) => !newChunkTexts.has(c.text),
    );
    if (chunksToDelete.length > 0) {
      this.logger?.debug(
        `  ${mdFile}: deleting ${chunksToDelete.length} obsolete chunks`,
      );
      const deleteVec = this.db.prepare(
        `DELETE FROM ${vecTableName} WHERE chunk_id = ?`,
      );
      const deleteChunk = this.db.prepare("DELETE FROM chunks WHERE id = ?");
      for (const chunk of chunksToDelete) {
        deleteVec.run(chunk.id);
        deleteChunk.run(chunk.id);
      }
    }

    // Find chunks that need to be embedded (new chunks not in existing map)
    const chunksToEmbed = chunks.filter((c) => !existingChunkMap.has(c.text));
    const reusedCount = chunks.length - chunksToEmbed.length;

    if (reusedCount > 0) {
      this.logger?.debug(`  ${mdFile}: reusing ${reusedCount} existing chunks`);
    }

    if (chunksToEmbed.length === 0) {
      this.logger?.debug(`  ${mdFile}: no new chunks to embed`);
      // Update file record with hash/mtime to mark indexing complete
      this.db
        .prepare<
          [number, string, number]
        >("UPDATE files SET mtime_ms = ?, hash = ? WHERE id = ?")
        .run(currentMtime, currentHash, fileId);
      this.indexLog.push({
        file: mdFile,
        chunkCount: 0,
        timestamp: new Date(),
      });
      if (this.indexLog.length > MAX_INDEX_LOG_ENTRIES) {
        this.indexLog = this.indexLog.slice(-MAX_INDEX_LOG_ENTRIES);
      }
      return;
    }

    this.logger?.info(
      `  ${mdFile}: embedding ${chunksToEmbed.length} new chunks`,
    );

    // Generate contextualized text for new chunks only
    const contextualizedTexts: string[] = [];
    for (let i = 0; i < chunksToEmbed.length; i++) {
      const chunk = chunksToEmbed[i];
      const contextParts: string[] = [];

      if (chunk.headingContext) {
        contextParts.push(chunk.headingContext);
      }

      if (this.ctx.llm) {
        const result = await generateContext(this.ctx.llm, content, chunk.text);
        contextParts.push(result.context);

        // Log progress and sample usage every 5 chunks
        if ((i + 1) % 5 === 0 || i === chunksToEmbed.length - 1) {
          const usage = result.usage;
          this.logger?.info(
            `  ${mdFile}: context ${i + 1}/${chunksToEmbed.length} (in: ${usage.inputTokens}, out: ${usage.outputTokens}, cache hits: ${usage.cacheHits ?? 0}, misses: ${usage.cacheMisses ?? 0})`,
          );
        }
      }

      if (contextParts.length > 0) {
        contextualizedTexts.push(
          `<context>\n${contextParts.join("\n\n")}\n</context>\n\n${chunk.text}`,
        );
      } else {
        contextualizedTexts.push(chunk.text);
      }
    }

    const embeddings =
      await this.ctx.embeddingModel.embedChunks(contextualizedTexts);

    const insertChunk = this.db.prepare(
      `INSERT INTO chunks (file_id, text, contextualized_text, start_line, start_col, end_line, end_col)
       VALUES (?, ?, ?, ?, ?, ?, ?)`,
    );
    const insertVec = this.db.prepare(
      `INSERT INTO ${vecTableName} (chunk_id, embedding) VALUES (?, ?)`,
    );

    for (let i = 0; i < chunksToEmbed.length; i++) {
      const chunk = chunksToEmbed[i];
      const result = insertChunk.run(
        fileId,
        chunk.text,
        contextualizedTexts[i],
        chunk.start.line,
        chunk.start.col,
        chunk.end.line,
        chunk.end.col,
      );
      const chunkId = Number(result.lastInsertRowid);
      insertVec.run(BigInt(chunkId), new Float32Array(embeddings[i]));
    }

    // Update file record with hash/mtime to mark indexing complete
    this.db
      .prepare<
        [number, string, number]
      >("UPDATE files SET mtime_ms = ?, hash = ? WHERE id = ?")
      .run(currentMtime, currentHash, fileId);

    this.indexLog.push({
      file: mdFile,
      chunkCount: chunksToEmbed.length,
      timestamp: new Date(),
    });

    if (this.indexLog.length > MAX_INDEX_LOG_ENTRIES) {
      this.indexLog = this.indexLog.slice(-MAX_INDEX_LOG_ENTRIES);
    }
  }

  getStats(): PKBStats {
    const fileCount = this.db
      .prepare<
        [string, number],
        { count: number }
      >("SELECT COUNT(*) as count FROM files WHERE model_name = ? AND embedding_version = ?")
      .get(this.ctx.embeddingModel.modelName, MAGENTA_EMBEDDING_VERSION);

    const chunkCount = this.db
      .prepare<
        [string, number],
        { count: number }
      >("SELECT COUNT(*) as count FROM chunks c JOIN files f ON c.file_id = f.id WHERE f.model_name = ? AND f.embedding_version = ?")
      .get(this.ctx.embeddingModel.modelName, MAGENTA_EMBEDDING_VERSION);

    return {
      totalFiles: fileCount?.count ?? 0,
      totalChunks: chunkCount?.count ?? 0,
      recentFiles: this.indexLog.slice(-5).reverse(),
    };
  }

  getAllChunks(): Array<{
    filename: PKBFile;
    text: string;
    contextualizedText: string;
  }> {
    const rows = this.db
      .prepare<
        [string, number],
        { filename: string; text: string; contextualized_text: string }
      >(
        `SELECT f.filename, c.text, c.contextualized_text
         FROM chunks c
         JOIN files f ON c.file_id = f.id
         WHERE f.model_name = ? AND f.embedding_version = ?
         ORDER BY f.filename, c.start_line, c.start_col`,
      )
      .all(this.ctx.embeddingModel.modelName, MAGENTA_EMBEDDING_VERSION);

    return rows.map((row) => ({
      filename: row.filename as PKBFile,
      text: row.text,
      contextualizedText: row.contextualized_text,
    }));
  }

  async search(query: string, topK: number = 10): Promise<SearchResult[]> {
    this.ensureVecTableInitialized();

    const queryEmbedding = await this.ctx.embeddingModel.embedQuery(query);
    const vecTableName = getVecTableName(
      this.ctx.embeddingModel.modelName,
      MAGENTA_EMBEDDING_VERSION,
    );

    const results = this.db
      .prepare<
        unknown[],
        {
          filename: string;
          text: string;
          contextualized_text: string;
          start_line: number;
          start_col: number;
          end_line: number;
          end_col: number;
          embedding_version: number;
          distance: number;
        }
      >(
        `SELECT
          f.filename,
          c.text,
          c.contextualized_text,
          c.start_line,
          c.start_col,
          c.end_line,
          c.end_col,
          f.embedding_version,
          v.distance
        FROM ${vecTableName} v
        JOIN chunks c ON c.id = v.chunk_id
        JOIN files f ON f.id = c.file_id
        WHERE v.embedding MATCH ? AND v.k = ?
        ORDER BY v.distance`,
      )
      .all(new Float32Array(queryEmbedding), topK);

    return results.map((row) => ({
      file: row.filename as PKBFile,
      chunk: {
        text: row.text,
        contextualizedText: row.contextualized_text,
        start: { line: row.start_line, col: row.start_col },
        end: { line: row.end_line, col: row.end_col },
        version: row.embedding_version,
      },
      score: (1 - row.distance) as Score,
    }));
  }
}
