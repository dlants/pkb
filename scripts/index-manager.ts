import * as fs from "fs";
import * as path from "path";
import {
  type PKB,
  type IndexedFileInfo,
  type PKBFile,
  type FileId,
  type MtimeMs,
  computeFileHash,
} from "./pkb.ts";
import type { AbsFilePath, TrackedSourceId } from "./db.ts";

export type Logger = {
  info: (msg: string) => void;
  debug: (msg: string) => void;
  error: (msg: string) => void;
};

export type FileOperation =
  | { type: "index"; filePath: AbsFilePath; trackedSourceId: TrackedSourceId }
  | { type: "delete"; filePath: AbsFilePath; fileId: FileId };

export type ScanResult = {
  operations: FileOperation[];
  skipped: AbsFilePath[];
};

const DEFAULT_UPDATE_INTERVAL_MS = 60000;

export class IndexManager {
  private indexQueue: FileOperation[] = [];
  private updateInterval: ReturnType<typeof setInterval> | undefined;
  private isProcessing = false;

  constructor(
    private pkb: PKB,
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
          `PKB update failed: ${e instanceof Error ? e.message : String(e)}`,
        );
      })
      .finally(() => {
        this.isProcessing = false;
      });
  }

  scanForChanges(): ScanResult {
    const operations: FileOperation[] = [];
    const skipped: AbsFilePath[] = [];

    const trackedSources = this.pkb.getTrackedSources();
    const indexedFiles = this.pkb.getIndexedFiles();
    const indexedFileMap = new Map<string, IndexedFileInfo>();
    for (const file of indexedFiles) {
      indexedFileMap.set(file.filename, file);
    }

    // Collect all expected files from tracked sources
    const expectedFiles = new Set<string>();

    for (const source of trackedSources) {
      const mdFiles = this.getMdFilesForSource(source.path, source.type);

      for (const mdPath of mdFiles) {
        expectedFiles.add(mdPath);

        const existingFile = indexedFileMap.get(mdPath);

        if (!fs.existsSync(mdPath)) {
          // File was deleted but tracked source still exists
          if (existingFile) {
            this.logger.info(
              `  ${mdPath}: file deleted, will remove embeddings`,
            );
            operations.push({
              type: "delete",
              filePath: mdPath as AbsFilePath,
              fileId: existingFile.id,
            });
          }
          continue;
        }

        const stat = fs.statSync(mdPath);
        const currentMtime = stat.mtimeMs as MtimeMs;

        if (existingFile) {
          if (existingFile.mtimeMs === currentMtime) {
            skipped.push(mdPath as AbsFilePath);
            continue;
          }

          const currentHash = computeFileHash(mdPath as AbsFilePath);
          if (existingFile.hash === currentHash) {
            this.pkb.updateFileMtime(existingFile.id, currentMtime);
            skipped.push(mdPath as AbsFilePath);
            continue;
          }

          operations.push({
            type: "index",
            filePath: mdPath as AbsFilePath,
            trackedSourceId: source.id,
          });
        } else {
          operations.push({
            type: "index",
            filePath: mdPath as AbsFilePath,
            trackedSourceId: source.id,
          });
        }
      }
    }

    // Find indexed files that are no longer in any tracked source
    for (const indexedFile of indexedFiles) {
      if (!expectedFiles.has(indexedFile.filename)) {
        this.logger.info(
          `  ${indexedFile.filename}: no longer tracked, will remove embeddings`,
        );
        operations.push({
          type: "delete",
          filePath: indexedFile.filename as unknown as AbsFilePath,
          fileId: indexedFile.id,
        });
      }
    }

    return { operations, skipped };
  }

  private getMdFilesForSource(
    sourcePath: AbsFilePath,
    sourceType: "file" | "directory",
  ): string[] {
    if (!fs.existsSync(sourcePath)) {
      return [];
    }

    if (sourceType === "file") {
      if (sourcePath.endsWith(".md")) {
        return [sourcePath];
      }
      return [];
    }

    // Directory: recursively find all .md files
    const mdFiles: string[] = [];
    const findMdFiles = (dir: string) => {
      const entries = fs.readdirSync(dir, { withFileTypes: true });
      for (const entry of entries) {
        const fullPath = path.join(dir, entry.name);
        if (entry.isDirectory()) {
          findMdFiles(fullPath);
        } else if (entry.isFile() && entry.name.endsWith(".md")) {
          mdFiles.push(fullPath);
        }
      }
    };
    findMdFiles(sourcePath);
    return mdFiles;
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
        this.pkb.deleteFile(operation.fileId);
        break;
      case "index":
        await this.pkb.indexFile(operation.filePath, operation.trackedSourceId);
        break;
    }

    return { status: "processed", operation };
  }

  async reindex(): Promise<void> {
    this.logger.info("PKB: Starting reindex...");

    const { operations, skipped } = this.scanForChanges();
    const toIndex = operations.filter((op) => op.type === "index").length;
    const toDelete = operations.filter((op) => op.type === "delete").length;

    this.logger.info(
      `PKB: ${toIndex} files to index, ${skipped.length} unchanged, ${toDelete} to delete`,
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
          `PKB: Indexed ${op.filePath} (${this.getQueueSize()} remaining)`,
        );
      } else {
        this.logger.info(
          `PKB: Deleted ${op.filePath} (${this.getQueueSize()} remaining)`,
        );
      }
    }

    this.logger.info(`PKB: Reindex complete. Processed ${processed} files.`);
  }

  getPKB(): PKB {
    return this.pkb;
  }
}
