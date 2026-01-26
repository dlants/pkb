import * as path from "path";
import { it, expect } from "vitest";
import { withTestHarness, respondToEmbedRequest } from "./test-harness.ts";

function normalizeChunksForSnapshot(
  chunks: Array<{ filename: string; text: string; contextualizedText: string }>,
) {
  return chunks.map((c) => ({
    ...c,
    filename: path.basename(c.filename),
  }));
}

it("should index new markdown files", async () => {
  await withTestHarness(async (ctx) => {
    await ctx.writeFile("test.md", "# Test Document\n\nThis is test content.");

    const reindexPromise = ctx.manager.reindex();

    // Respond to context generation request
    const llmReq = await ctx.mockLLM.awaitPendingRequest();
    ctx.mockLLM.respondTo(llmReq, "Context for test document");

    // Respond to embedding request
    const embedReq = await ctx.mockEmbed.awaitPendingRequest();
    expect(embedReq.type).toBe("chunks");
    const chunks = embedReq.input as string[];
    expect(chunks.length).toBeGreaterThan(0);
    respondToEmbedRequest(
      embedReq,
      chunks.map(() => [0.1, 0.2, 0.3]),
    );

    await reindexPromise;

    const stats = ctx.pkb.getStats();
    expect(stats.totalFiles).toBe(1);
    expect(stats.totalChunks).toBeGreaterThan(0);

    const allChunks = ctx.pkb.getAllChunks();
    expect(normalizeChunksForSnapshot(allChunks)).toMatchSnapshot();
  });
});

it("should skip files that haven't changed on reindex", async () => {
  await withTestHarness(async (ctx) => {
    await ctx.writeFile("test.md", "# Test Document\n\nThis is test content.");

    // Initial indexing
    const reindexPromise = ctx.manager.reindex();
    const llmReq = await ctx.mockLLM.awaitPendingRequest();
    ctx.mockLLM.respondTo(llmReq, "Context for test");
    const embedReq = await ctx.mockEmbed.awaitPendingRequest();
    const chunks = embedReq.input as string[];
    respondToEmbedRequest(
      embedReq,
      chunks.map(() => [0.1, 0.2, 0.3]),
    );
    await reindexPromise;

    const initialRequestCount = ctx.mockEmbed.requests.length;

    // Trigger reindex again
    await ctx.manager.reindex();

    // Should not have made any new embed requests since file hasn't changed
    expect(ctx.mockEmbed.requests.length).toBe(initialRequestCount);
  });
});

it("should only re-embed changed chunks when file content changes", async () => {
  const originalContent = `# Section One

This is the first section content.

# Section Two

This is the second section content.

# Section Three

This is the third section content.`;

  const modifiedContent = `# Section One

This is the first section content.

# Section Two

This section has been MODIFIED with new content.

# Section Three

This is the third section content.`;

  await withTestHarness(async (ctx) => {
    await ctx.writeFile("test.md", originalContent);

    // Initial indexing
    const reindexPromise1 = ctx.manager.reindex();

    // Respond with unique context for each chunk
    for (const contextText of [
      "Section one introduces the document",
      "Section two provides secondary content",
      "Section three concludes the document",
    ]) {
      const llmReq = await ctx.mockLLM.awaitPendingRequest();
      ctx.mockLLM.respondTo(llmReq, contextText);
    }

    const embedReq = await ctx.mockEmbed.awaitPendingRequest();
    const initialChunks = embedReq.input as string[];
    expect(initialChunks.length).toBe(3);
    expect(initialChunks.some((c) => c.includes("first section"))).toBe(true);
    expect(initialChunks.some((c) => c.includes("second section"))).toBe(true);
    expect(initialChunks.some((c) => c.includes("third section"))).toBe(true);
    respondToEmbedRequest(
      embedReq,
      initialChunks.map((_, i) => [0.1 * (i + 1), 0.2 * (i + 1), 0.3]),
    );
    await reindexPromise1;

    const statsAfterInitial = ctx.pkb.getStats();
    expect(statsAfterInitial.totalChunks).toBe(3);

    const chunksAfterInitial = ctx.pkb.getAllChunks();
    expect(normalizeChunksForSnapshot(chunksAfterInitial)).toMatchSnapshot(
      "after initial indexing",
    );

    // Modify only section two
    await ctx.writeFile("test.md", modifiedContent);

    // Trigger reindex again
    const reindexPromise2 = ctx.manager.reindex();

    // Only section two needs new context
    const llmReq2 = await ctx.mockLLM.awaitPendingRequest();
    ctx.mockLLM.respondTo(llmReq2, "Section two now contains modified content");

    const updateReq = await ctx.mockEmbed.awaitPendingRequest();
    const updatedChunks = updateReq.input as string[];

    // Should only embed 1 new chunk (the modified section)
    expect(updatedChunks.length).toBe(1);
    expect(updatedChunks[0]).toContain("MODIFIED");

    respondToEmbedRequest(updateReq, [[0.4, 0.5, 0.6]]);
    await reindexPromise2;

    // Total chunks should still be 3 (2 reused + 1 new, old one deleted)
    const statsAfterUpdate = ctx.pkb.getStats();
    expect(statsAfterUpdate.totalChunks).toBe(3);

    const chunksAfterUpdate = ctx.pkb.getAllChunks();
    expect(normalizeChunksForSnapshot(chunksAfterUpdate)).toMatchSnapshot(
      "after modifying section two",
    );
  });
});

it("should delete embeddings when file is removed", async () => {
  await withTestHarness(async (ctx) => {
    await ctx.writeFile("test.md", "# Test Document\n\nThis is test content.");

    // Initial indexing
    const reindexPromise = ctx.manager.reindex();
    const llmReq = await ctx.mockLLM.awaitPendingRequest();
    ctx.mockLLM.respondTo(llmReq, "Context for test");
    const embedReq = await ctx.mockEmbed.awaitPendingRequest();
    const chunks = embedReq.input as string[];
    respondToEmbedRequest(
      embedReq,
      chunks.map(() => [0.1, 0.2, 0.3]),
    );
    await reindexPromise;

    const statsAfterIndex = ctx.pkb.getStats();
    expect(statsAfterIndex.totalFiles).toBe(1);
    expect(statsAfterIndex.totalChunks).toBeGreaterThan(0);

    // Delete the file
    await ctx.deleteFile("test.md");

    // Trigger reindex
    await ctx.manager.reindex();

    const statsAfterDelete = ctx.pkb.getStats();
    expect(statsAfterDelete.totalFiles).toBe(0);
    expect(statsAfterDelete.totalChunks).toBe(0);
  });
});
