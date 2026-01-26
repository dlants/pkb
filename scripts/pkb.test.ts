import { describe, it, expect } from "vitest";
import * as fs from "fs/promises";
import * as path from "path";
import { PKB } from "./pkb.ts";
import { initDatabase, type AbsFilePath } from "./db.ts";
import { MockEmbeddingModel, respondToEmbedRequest } from "./embedding/mock.ts";
import { MockLLM } from "./llm.ts";

describe("PKB", () => {
  it("should export PKB class", () => {
    expect(PKB).toBeDefined();
  });
});

describe("tracked sources", () => {
  async function withTempPKB(
    fn: (ctx: {
      pkb: PKB;
      tmpDir: string;
      mockEmbed: MockEmbeddingModel;
      mockLLM: MockLLM;
    }) => Promise<void>,
  ) {
    const testId = Math.random().toString(36).substring(2, 15);
    const tmpDir = path.join("/tmp/pkb-test", testId);
    const dbPath = path.join(tmpDir, "pkb.db") as AbsFilePath;

    await fs.mkdir(tmpDir, { recursive: true });

    const db = initDatabase(dbPath);
    const mockEmbed = new MockEmbeddingModel();
    const mockLLM = new MockLLM();

    const pkb = new PKB({ db, embeddingModel: mockEmbed, llm: mockLLM });

    try {
      await fn({ pkb, tmpDir, mockEmbed, mockLLM });
    } finally {
      pkb.close();
      await fs.rm(tmpDir, { recursive: true, force: true });
    }
  }

  it("should add and list tracked sources", async () => {
    await withTempPKB(async ({ pkb, tmpDir }) => {
      const filesDir = path.join(tmpDir, "files") as AbsFilePath;
      await fs.mkdir(filesDir);

      const source = pkb.addTrackedSource(filesDir, "directory");
      expect(source.path).toBe(filesDir);
      expect(source.type).toBe("directory");
      expect(source.id).toBeDefined();

      const sources = pkb.getTrackedSources();
      expect(sources.length).toBe(1);
      expect(sources[0].path).toBe(filesDir);
    });
  });

  it("should track individual files", async () => {
    await withTempPKB(async ({ pkb, tmpDir }) => {
      const filePath = path.join(tmpDir, "note.md") as AbsFilePath;
      await fs.writeFile(filePath, "# Note\n\nSome content");

      const source = pkb.addTrackedSource(filePath, "file");
      expect(source.type).toBe("file");

      const sources = pkb.getTrackedSources();
      expect(sources.length).toBe(1);
      expect(sources[0].type).toBe("file");
    });
  });

  it("should remove tracked source and delete associated files", async () => {
    await withTempPKB(async ({ pkb, tmpDir, mockEmbed, mockLLM }) => {
      const filesDir = path.join(tmpDir, "files") as AbsFilePath;
      await fs.mkdir(filesDir);
      await fs.writeFile(
        path.join(filesDir, "test.md"),
        "# Test\n\nContent here",
      );

      const source = pkb.addTrackedSource(filesDir, "directory");

      // Index the file
      const filePath = path.join(filesDir, "test.md") as AbsFilePath;
      const indexPromise = pkb.indexFile(filePath, source.id);

      const llmReq = await mockLLM.awaitPendingRequest();
      mockLLM.respondTo(llmReq, "Test context");

      const embedReq = await mockEmbed.awaitPendingRequest();
      respondToEmbedRequest(embedReq, [[0.1, 0.2, 0.3]]);

      await indexPromise;

      expect(pkb.getStats().totalFiles).toBe(1);

      // Remove the tracked source
      pkb.removeTrackedSource(filesDir);

      expect(pkb.getTrackedSources().length).toBe(0);
      expect(pkb.getStats().totalFiles).toBe(0);
    });
  });

  it("should handle multiple tracked sources", async () => {
    await withTempPKB(async ({ pkb, tmpDir }) => {
      const dir1 = path.join(tmpDir, "dir1") as AbsFilePath;
      const dir2 = path.join(tmpDir, "dir2") as AbsFilePath;
      const file1 = path.join(tmpDir, "standalone.md") as AbsFilePath;

      await fs.mkdir(dir1);
      await fs.mkdir(dir2);
      await fs.writeFile(file1, "# Standalone");

      pkb.addTrackedSource(dir1, "directory");
      pkb.addTrackedSource(dir2, "directory");
      pkb.addTrackedSource(file1, "file");

      const sources = pkb.getTrackedSources();
      expect(sources.length).toBe(3);
    });
  });
});

// import { it, expect } from "vitest";
// import * as fs from "fs/promises";
// import * as path from "path";
// import { withDriver } from "../test/preamble.ts";
// import { respondToEmbedRequest } from "./embedding/mock.ts";
// import { pollUntil } from "../utils/async.ts";
// import type { NvimDriver } from "../test/driver.ts";
//
// async function respondToAllContextRequests(driver: NvimDriver) {
//   let count = 0;
//   while (true) {
//     const pendingRequest = driver.mockAnthropic.textRequests.find(
//       (r) => !r.defer.resolved,
//     );
//     if (!pendingRequest) break;
//     pendingRequest.defer.resolve({
//       text: `Context for chunk ${count + 1}`,
//       stopReason: "end_turn",
//       usage: { inputTokens: 10, outputTokens: 20 },
//     });
//     count++;
//     await driver.wait(10);
//   }
//   return count;
// }
//
// async function awaitEmbedRequestWithContextResponses(driver: NvimDriver) {
//   return pollUntil(async () => {
//     await respondToAllContextRequests(driver);
//     const embedReq = driver.mockEmbed!.requests.find((r) => !r.defer.resolved);
//     if (!embedReq) {
//       throw new Error("waiting for embedChunks call");
//     }
//     return embedReq;
//   });
// }
//
// it("should search and return relevant chunks", async () => {
//   await withDriver(
//     {
//       setupFiles: async (tmpDir) => {
//         const pkbDir = path.join(tmpDir, "pkb");
//         await fs.mkdir(pkbDir);
//         await fs.writeFile(
//           path.join(pkbDir, "doc1.md"),
//           "# Apples\n\nApples are red fruits that grow on trees.",
//         );
//         await fs.writeFile(
//           path.join(pkbDir, "doc2.md"),
//           "# Oranges\n\nOranges are orange colored citrus fruits.",
//         );
//       },
//       options: {
//         pkb: {
//           path: "./pkb",
//           embeddingModel: { provider: "mock" },
//         },
//       },
//     },
//     async (driver) => {
//       expect(driver.mockEmbed).toBeDefined();
//
//       // Trigger reindex to index both files
//       const reindexPromise = driver.magenta.pkbManager!.reindex();
//
//       // First file embedding
//       const embedReq1 = await awaitEmbedRequestWithContextResponses(driver);
//       const chunks1 = embedReq1.input as string[];
//       respondToEmbedRequest(
//         embedReq1,
//         chunks1.map(() => [0.9, 0.1, 0.1]), // apple-like embedding
//       );
//
//       // Second file embedding
//       const embedReq2 = await awaitEmbedRequestWithContextResponses(driver);
//       const chunks2 = embedReq2.input as string[];
//       respondToEmbedRequest(
//         embedReq2,
//         chunks2.map(() => [0.1, 0.9, 0.1]), // orange-like embedding
//       );
//
//       await reindexPromise;
//
//       // Search
//       const searchPromise = driver.magenta.pkb!.search("apple fruit", 5);
//       const queryReq = await driver.mockEmbed!.awaitPendingRequest();
//       respondToEmbedRequest(queryReq, [0.9, 0.1, 0.1]); // apple-like query
//       const results = await searchPromise;
//
//       expect(results.length).toBeGreaterThan(0);
//       expect(results[0].score).toBeGreaterThan(0);
//       expect(results[0].file).toBeDefined();
//       expect(results[0].chunk.text).toBeDefined();
//     },
//   );
// });
//
// it("should return empty results for empty PKB", async () => {
//   await withDriver(
//     {
//       setupFiles: async (tmpDir) => {
//         const pkbDir = path.join(tmpDir, "pkb");
//         await fs.mkdir(pkbDir);
//         // No files in pkb directory
//       },
//       options: {
//         pkb: {
//           path: "./pkb",
//           embeddingModel: { provider: "mock" },
//         },
//       },
//     },
//     async (driver) => {
//       expect(driver.mockEmbed).toBeDefined();
//
//       // Search with no indexed files
//       const searchPromise = driver.magenta.pkb!.search("anything", 5);
//       const queryReq = await driver.mockEmbed!.awaitPendingRequest();
//       respondToEmbedRequest(queryReq, [0.5, 0.5, 0.5]);
//       const results = await searchPromise;
//
//       expect(results).toHaveLength(0);
//     },
//   );
// });
