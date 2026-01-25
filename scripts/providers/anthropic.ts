import Anthropic from "@anthropic-ai/sdk";

export type Usage = {
  inputTokens: number;
  outputTokens: number;
  cacheHits?: number;
  cacheMisses?: number;
};

export type AgentInput =
  | { type: "text"; text: string }
  | { type: "image"; source: Anthropic.Messages.Base64ImageSource }
  | {
      type: "document";
      source: Anthropic.Messages.Base64PDFSource;
      title?: string;
    };

export type ProviderTextRequest = {
  promise: Promise<{
    text: string;
    stopReason: string;
    usage: Usage;
  }>;
  aborted: boolean;
  abort: () => void;
};

export type Provider = {
  request(options: {
    model: string;
    input: AgentInput[];
    systemPrompt?: string;
  }): ProviderTextRequest;
};

function assertUnreachable(_x: never): never {
  throw new Error("Didn't expect to get here");
}

function getMaxTokensForModel(model: string): number {
  if (model.includes("haiku")) {
    return 8192;
  }
  return 16384;
}

function withCacheControl(
  messages: Anthropic.MessageParam[],
): Anthropic.MessageParam[] {
  return messages.map((msg, idx) => {
    if (idx === messages.length - 1 && Array.isArray(msg.content)) {
      const content = msg.content.map((block, blockIdx) => {
        if (blockIdx === msg.content.length - 1) {
          return { ...block, cache_control: { type: "ephemeral" as const } };
        }
        return block;
      });
      return { ...msg, content };
    }
    return msg;
  });
}

export class AnthropicProvider implements Provider {
  protected client: Anthropic;

  constructor() {
    this.client = new Anthropic();
  }

  request(options: {
    model: string;
    input: AgentInput[];
    systemPrompt?: string;
  }): ProviderTextRequest {
    const { model, input, systemPrompt } = options;
    let aborted = false;

    const userContent: Anthropic.Messages.ContentBlockParam[] = input.map(
      (c): Anthropic.Messages.ContentBlockParam => {
        switch (c.type) {
          case "text":
            return { type: "text", text: c.text, citations: null };
          case "image":
            return { type: "image", source: c.source };
          case "document":
            return {
              type: "document",
              source: c.source,
              title: c.title || null,
            };
          default:
            assertUnreachable(c);
        }
      },
    );

    const messages: Anthropic.MessageParam[] = [
      { role: "user", content: userContent },
    ];

    const streamParams: Anthropic.Messages.MessageStreamParams = {
      model,
      max_tokens: getMaxTokensForModel(model),
      messages: withCacheControl(messages),
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

    const request = this.client.messages.stream(streamParams);

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
      aborted,
      abort: () => {
        aborted = true;
        request.abort();
      },
    };
  }
}
