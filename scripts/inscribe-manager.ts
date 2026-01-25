import * as fs from "fs";
import * as path from "path";
import {
  type Grimoire,
  type FileOperation,
  type IndexedFileInfo,
  type Spell,
  type MtimeMs,
  computeFileHash,
} from "./grimoire.ts";
import type { AbsFilePath } from "./db.ts";

export type Logger = {
  info: (msg: string) => void;
  debug: (msg: string) => void;
  error: (msg: string) => void;
};

export type ScanResult = {
  operations: FileOperation[];
  skipped: Spell[];
};

const DEFAULT_UPDATE_INTERVAL_MS = 60000;

export class InscribeManager {
  private indexQueue: FileOperation[] = [];
  private updateInterval: ReturnType<typeof setInterval> | undefined;
  private isProcessing = false;

  constructor(
    private ctx: { spellsDir: AbsFilePath },
    private grimoire: Grimoire,
    private logger: Logger,
    private intervalMs: number = DEFAULT_UPDATE_INTERVAL_MS,
  ) {}

  start(): void {
    this.runUpdate();

    this.updateInterval = setInterval(() => {
      this.runUpdate();
    }, this.intervalMs);
  }

  stop(): void {
    if (this.updateInterval) {
      clearInterval(this.updateInterval);
      this.updateInterval = undefined;
    }
  }

  private runUpdate(): void {
    if (this.isProcessing) {
      return;
    }

    this.isProcessing = true;
    this.reindex()
      .catch((e) => {
        this.logger.error(
          `Grimoire update failed: ${e instanceof Error ? e.message : String(e)}`,
        );
      })
      .finally(() => {
        this.isProcessing = false;
      });
  }

  scanForChanges(): ScanResult {
    const operations: FileOperation[] = [];
    const skipped: Spell[] = [];

    const spellsDir = this.ctx.spellsDir;
    const files = fs.readdirSync(spellsDir);
    const mdFiles = files.filter((f) => f.endsWith(".md")) as Spell[];
    const mdFileSet = new Set(mdFiles);

    const indexedFiles = this.grimoire.getIndexedFiles();
    const indexedFileMap = new Map<Spell, IndexedFileInfo>();
    for (const file of indexedFiles) {
      indexedFileMap.set(file.filename, file);
    }

    for (const indexedFile of indexedFiles) {
      if (!mdFileSet.has(indexedFile.filename)) {
        this.logger.info(
          `  ${indexedFile.filename}: file deleted, will remove embeddings`,
        );
        operations.push({
          type: "delete",
          filename: indexedFile.filename,
          fileId: indexedFile.id,
        });
      }
    }

    for (const mdFile of mdFiles) {
      const mdPath = path.join(spellsDir, mdFile) as AbsFilePath;
      const stat = fs.statSync(mdPath);
      const currentMtime = stat.mtimeMs as MtimeMs;

      const existingFile = indexedFileMap.get(mdFile);

      if (existingFile) {
        if (existingFile.mtimeMs === currentMtime) {
          skipped.push(mdFile);
          continue;
        }

        const currentHash = computeFileHash(mdPath);
        if (existingFile.hash === currentHash) {
          this.grimoire.updateFileMtime(existingFile.id, currentMtime);
          skipped.push(mdFile);
          continue;
        }

        operations.push({ type: "index", filename: mdFile });
      } else {
        operations.push({ type: "index", filename: mdFile });
      }
    }

    return { operations, skipped };
  }

  queueOperations(operations: FileOperation[]): void {
    this.indexQueue.push(...operations);
  }

  getQueueSize(): number {
    return this.indexQueue.length;
  }

  async processNextInQueue(): Promise<
    { status: "processed"; operation: FileOperation } | { status: "empty" }
  > {
    const operation = this.indexQueue.shift();
    if (!operation) {
      return { status: "empty" };
    }

    switch (operation.type) {
      case "delete":
        this.grimoire.deleteFile(operation.fileId);
        break;
      case "index":
        await this.grimoire.indexFile(operation.filename);
        break;
    }

    return { status: "processed", operation };
  }

  async reindex(): Promise<void> {
    this.logger.info("Grimoire: Starting reindex...");

    const { operations, skipped } = this.scanForChanges();
    const toIndex = operations.filter((op) => op.type === "index").length;
    const toDelete = operations.filter((op) => op.type === "delete").length;

    this.logger.info(
      `Grimoire: ${toIndex} files to index, ${skipped.length} unchanged, ${toDelete} to delete`,
    );

    this.queueOperations(operations);

    let processed = 0;
    while (true) {
      const result = await this.processNextInQueue();
      if (result.status === "empty") {
        break;
      }
      processed++;
      const op = result.operation;
      if (op.type === "index") {
        this.logger.info(
          `Grimoire: Indexed ${op.filename} (${this.getQueueSize()} remaining)`,
        );
      } else {
        this.logger.info(
          `Grimoire: Deleted ${op.filename} (${this.getQueueSize()} remaining)`,
        );
      }
    }

    this.logger.info(`Grimoire: Reindex complete. Processed ${processed} files.`);
  }

  getGrimoire(): Grimoire {
    return this.grimoire;
  }
}
