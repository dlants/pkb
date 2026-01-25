import type { SearchResult } from "./pkb/pkb.ts";

export type SearchInput = {
  query: string;
  topK?: number;
};

export function formatResults(results: SearchResult[]): string {
  if (results.length === 0) {
    return "No results found.";
  }

  return results
    .map(
      (r, i) =>
        `## Result ${i + 1} (score: ${r.score.toFixed(3)})\nFile: ${r.file}\nLines ${r.chunk.start.line}-${r.chunk.end.line}\n\n${r.chunk.contextualizedText}`,
    )
    .join("\n\n---\n\n");
}
