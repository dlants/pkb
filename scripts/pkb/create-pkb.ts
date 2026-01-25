import * as os from "os";
import * as path from "path";
import { PKB } from "./pkb.ts";
import { BedrockCohereEmbedding } from "./embedding/bedrock-cohere.ts";
import type { EmbeddingModel } from "./embedding/types.ts";
import {
  MockEmbeddingModel,
  getMockEmbeddingModel,
  setMockEmbeddingModel,
} from "./embedding/mock.ts";
import type { Provider } from "../providers/anthropic.ts";
import type { Logger } from "./pkb-manager.ts";

export const DEFAULT_PKB_PATH = path.join(os.homedir(), "pkb");

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

function expandTilde(filePath: string): string {
  if (filePath.startsWith("~/")) {
    return path.join(os.homedir(), filePath.slice(2));
  }
  if (filePath === "~") {
    return os.homedir();
  }
  return filePath;
}

export type CreatePKBOptions = {
  pkbPath: string;
  embeddingModel: EmbeddingModelOptions;
  provider: Provider;
  model: string;
  logger?: Logger;
};

export function createPKB(options: CreatePKBOptions): PKB {
  const embeddingModel = createEmbeddingModel(options.embeddingModel);
  const expandedPath = expandTilde(options.pkbPath);
  const resolvedPath = path.resolve(expandedPath);

  return new PKB(
    resolvedPath,
    embeddingModel,
    { provider: options.provider, model: options.model },
    { logger: options.logger },
  );
}
