import { PKB } from "./pkb.ts";
import { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
import type { EmbeddingModel } from "./embedding/types.ts";
import {
  MockEmbeddingModel,
  getMockEmbeddingModel,
  setMockEmbeddingModel,
} from "./embedding/mock.ts";
import type { LLM } from "./llm.ts";
import type { Logger } from "./index-manager.ts";
import { createContext, type PKBOptions } from "./context.ts";

export type EmbeddingModelOptions =
  | { provider: "mock" }
  | { provider: "bedrock"; model: "cohere.embed-v4:0"; region?: string };

export function createEmbeddingModel(
  options: EmbeddingModelOptions,
): EmbeddingModel {
  if (options.provider === "mock") {
    let mockModel = getMockEmbeddingModel();
    if (!mockModel) {
      mockModel = new MockEmbeddingModel();
      setMockEmbeddingModel(mockModel);
    }
    return mockModel;
  }

  if (options.provider === "bedrock" && options.model === "cohere.embed-v4:0") {
    return new BedrockCohereEmbedding({ region: options.region });
  }

  throw new Error(`Unknown embedding model: ${JSON.stringify(options)}`);
}

export type CreatePKBFactoryOptions = {
  pkbOptions: PKBOptions;
  embeddingModel: EmbeddingModelOptions;
  llm?: LLM;
  logger?: Logger;
};

export function createPKBFromOptions(options: CreatePKBFactoryOptions): PKB {
  const embeddingModel = createEmbeddingModel(options.embeddingModel);
  const pkbOptions = options.pkbOptions;
  const ctx = createContext(pkbOptions, embeddingModel, options.llm);

  return new PKB(ctx, { logger: options.logger });
}
