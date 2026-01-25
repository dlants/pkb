export type Embedding = number[];

export type EmbeddingModelName = "cohere.embed-v4:0" | "mock-embedding";
export type EmbeddingVersion = 3;

export const MAGENTA_EMBEDDING_VERSION: EmbeddingVersion = 3;

export interface EmbeddingModel {
  modelName: EmbeddingModelName;
  dimensions: number;
  embedChunk(chunk: string): Promise<Embedding>;
  embedQuery(query: string): Promise<Embedding>;
  embedChunks(chunks: string[]): Promise<Embedding[]>;
}

export type Position = {
  line: number;
  col: number;
};

export type ChunkData = {
  text: string;
  contextualizedText: string;
  start: Position;
  end: Position;
  version: number;
};
