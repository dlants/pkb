export { PKB } from "./pkb.ts";
export type { SearchResult } from "./pkb.ts";
export { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
export type {
  EmbeddingModel,
  Embedding,
  ChunkData,
  Position,
} from "./embedding/types.ts";
export { chunkMarkdown } from "./chunker.ts";
export type { ChunkInfo } from "./chunker.ts";
export { createPKBFromOptions, createEmbeddingModel } from "./create-pkb.ts";
export { DEFAULT_DB_PATH, DEFAULT_FILES_DIR } from "./db.ts";
export type { PKBOptions, PKBContext } from "./context.ts";
export { IndexManager } from "./index-manager.ts";
