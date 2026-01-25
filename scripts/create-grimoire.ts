import { Grimoire } from "./grimoire.ts";
import { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
import type { EmbeddingModel } from "./embedding/types.ts";
import {
  MockEmbeddingModel,
  getMockEmbeddingModel,
  setMockEmbeddingModel,
} from "./embedding/mock.ts";
import type { LLM } from "./llm.ts";
import type { Logger } from "./inscribe-manager.ts";
import { createContext, DEFAULT_OPTIONS, type GrimoireOptions } from "./context.ts";

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

export type CreateGrimoireFactoryOptions = {
  embeddingModel: EmbeddingModelOptions;
  llm?: LLM;
  logger?: Logger;
  grimoireOptions?: GrimoireOptions;
};

export function createGrimoireFromOptions(options: CreateGrimoireFactoryOptions): Grimoire {
  const embeddingModel = createEmbeddingModel(options.embeddingModel);
  const grimoireOptions = options.grimoireOptions ?? DEFAULT_OPTIONS;
  const ctx = createContext(grimoireOptions, embeddingModel, options.llm);

  return new Grimoire(ctx, { logger: options.logger });
}
