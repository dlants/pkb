import { AnthropicBedrock } from "@anthropic-ai/bedrock-sdk";
import type Anthropic from "@anthropic-ai/sdk";
import { Defer, pollUntil } from "./utils/async.ts";


export type Usage = {
  inputTokens: number;
  outputTokens: number;
  cacheHits?: number;
  cacheMisses?: number;
};

export type LLMRequest = {
  promise: Promise<{
    text: string;
    stopReason: string;
    usage: Usage;
  }>;
  abort: () => void;
};

export type LLM = {
  request(options: { input: string; systemPrompt?: string }): LLMRequest;
};

const MODEL = "us.anthropic.claude-haiku-4-5-20251001-v1:0";
const MAX_TOKENS = 8192;

export function createBedrockHaikuLLM(): LLM {
  const client = new AnthropicBedrock();

  return {
    request({ input, systemPrompt }): LLMRequest {
      const messages: Anthropic.MessageParam[] = [
        { role: "user", content: [{ type: "text", text: input }] },
      ];

      const streamParams: Anthropic.Messages.MessageStreamParams = {
        model: MODEL,
        max_tokens: MAX_TOKENS,
        messages: messages.map((msg, idx) => {
          if (idx === messages.length - 1 && Array.isArray(msg.content)) {
            const content = msg.content.map((block, blockIdx) => {
              if (blockIdx === msg.content.length - 1) {
                return {
                  ...block,
                  cache_control: { type: "ephemeral" as const },
                };
              }
              return block;
            });
            return { ...msg, content };
          }
          return msg;
        }),
      };

      if (systemPrompt) {
        streamParams.system = [
          {
            type: "text" as const,
            text: systemPrompt,
            cache_control: { type: "ephemeral" },
          },
        ];
      }

      const request = client.messages.stream(streamParams);

      const promise = (async () => {
        const response: Anthropic.Message = await request.finalMessage();

        let text = "";
        for (const block of response.content) {
          if (block.type === "text") {
            text += block.text;
          }
        }

        const usage: Usage = {
          inputTokens: response.usage.input_tokens,
          outputTokens: response.usage.output_tokens,
        };
        if (response.usage.cache_read_input_tokens != undefined) {
          usage.cacheHits = response.usage.cache_read_input_tokens;
        }
        if (response.usage.cache_creation_input_tokens != undefined) {
          usage.cacheMisses = response.usage.cache_creation_input_tokens;
        }

        return {
          text,
          stopReason: response.stop_reason || "end_turn",
          usage,
        };
      })();

      return {
        promise,
        abort: () => request.abort(),
      };
    },
  };
}

export type MockLLMRequest = {
  input: string;
  systemPrompt?: string;
  defer: Defer<{ text: string; stopReason: string; usage: Usage }>;
};

export class MockLLM implements LLM {
  public requests: MockLLMRequest[] = [];

  request(options: { input: string; systemPrompt?: string }): LLMRequest {
    const defer = new Defer<{ text: string; stopReason: string; usage: Usage }>();
    const req: MockLLMRequest = {
      input: options.input,
      systemPrompt: options.systemPrompt,
      defer,
    };
    this.requests.push(req);

    return {
      promise: defer.promise,
      abort: () => {
        if (!defer.resolved) {
          defer.reject(new Error("aborted"));
        }
      },
    };
  }

  async awaitPendingRequest(message?: string): Promise<MockLLMRequest> {
    return pollUntil(
      () => {
        const pending = this.requests.find((r) => !r.defer.resolved);
        if (!pending) {
          throw new Error(
            `No pending LLM requests. ${message ?? ""} Total: ${this.requests.length}`,
          );
        }
        return pending;
      },
      { timeout: 2000 },
    );
  }

  respondTo(
    req: MockLLMRequest,
    text: string,
    usage: Usage = { inputTokens: 10, outputTokens: 20 },
  ): void {
    req.defer.resolve({ text, stopReason: "end_turn", usage });
  }
}

