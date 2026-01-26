import { describe, it, expect } from "vitest";
import { formatResults } from "./search.ts";
import { withTestHarness, respondToEmbedRequest } from "./test-harness.ts";

describe("formatResults", () => {
  it("should return no results message for empty array", () => {
    expect(formatResults([])).toBe("No results found.");
  });
});

describe("search", () => {
  it("should search and return relevant chunks", async () => {
    await withTestHarness(async (ctx) => {
      await ctx.writeFile(
        "notes.md",
        `# Project Documentation

## Architecture Overview
The system uses a microservices architecture with REST APIs for communication between services.
Each service is independently deployable and scalable, following the principles of domain-driven design.

## Database Design
We use PostgreSQL for persistent storage of relational data.
The database follows a multi-tenant architecture with row-level security.

## Security Considerations
Authentication is handled via OAuth 2.0 with JWT tokens.
All API endpoints require authentication except for health checks.`,
      );

      // Index the file
      const reindexPromise = ctx.manager.reindex();

      // Respond to context generation for each chunk
      for (const contextText of [
        "Architecture section about microservices",
        "Database section about PostgreSQL",
        "Security section about authentication",
      ]) {
        const llmReq = await ctx.mockLLM.awaitPendingRequest();
        ctx.mockLLM.respondTo(llmReq, contextText);
      }

      // Respond with embeddings that will produce distinct similarity scores:
      // - Chunk 0 (architecture): [1, 0, 0] - closest to query
      // - Chunk 1 (database): [0.3, 0.3, 0.3] - medium similarity
      // - Chunk 2 (security): [0, 0, 1] - orthogonal to query
      const embedReq = await ctx.mockEmbed.awaitPendingRequest();
      respondToEmbedRequest(embedReq, [
        [1, 0, 0],
        [0.3, 0.3, 0.3],
        [0, 0, 1],
      ]);
      await reindexPromise;

      // Now search for architecture
      const searchPromise = ctx.pkb.search("architecture", 5);

      // Respond to query embedding - similar to architecture chunk
      const queryReq = await ctx.mockEmbed.awaitPendingRequest();
      expect(queryReq.type).toBe("query");
      expect(queryReq.input).toBe("architecture");
      respondToEmbedRequest(queryReq, [1, 0, 0]);

      const results = await searchPromise;

      // Should return results ordered by similarity
      expect(results.length).toBe(3);
      expect(results[0].chunk.text).toContain("microservices architecture");
      expect(results[0].score).toBeGreaterThan(results[1].score);

      // Format results and verify output
      const formatted = formatResults(results);
      expect(formatted).toContain("## Result 1");
      expect(formatted).toContain("notes.md");
      expect(formatted).toContain("Architecture");
    });
  });

  it("should re-embed file when content changes", async () => {
    await withTestHarness(async (ctx) => {
      await ctx.writeFile(
        "notes.md",
        "# Original Content\nThis is the initial content.",
      );

      // Initial index
      const reindexPromise1 = ctx.manager.reindex();
      const llmReq1 = await ctx.mockLLM.awaitPendingRequest();
      ctx.mockLLM.respondTo(llmReq1, "Original content context");
      const embedReq1 = await ctx.mockEmbed.awaitPendingRequest();
      const chunks1 = embedReq1.input as string[];
      expect(chunks1[0]).toContain("initial content");
      respondToEmbedRequest(embedReq1, [[0.1, 0.2, 0.3]]);
      await reindexPromise1;

      // Modify the file
      await ctx.writeFile(
        "notes.md",
        "# Updated Content\nThis content has been modified.",
      );

      // Reindex
      const reindexPromise2 = ctx.manager.reindex();
      const llmReq2 = await ctx.mockLLM.awaitPendingRequest();
      ctx.mockLLM.respondTo(llmReq2, "Updated content context");
      const embedReq2 = await ctx.mockEmbed.awaitPendingRequest();
      const chunks2 = embedReq2.input as string[];
      expect(chunks2[0]).toContain("has been modified");
      respondToEmbedRequest(embedReq2, [[0.4, 0.5, 0.6]]);
      await reindexPromise2;

      // Verify the updated content is searchable
      const searchPromise = ctx.pkb.search("updated", 5);
      const queryReq = await ctx.mockEmbed.awaitPendingRequest();
      respondToEmbedRequest(queryReq, [0.4, 0.5, 0.6]);
      const results = await searchPromise;

      expect(results.length).toBe(1);
      expect(results[0].chunk.text).toContain("has been modified");
    });
  });
});
