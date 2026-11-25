import type { Input, Kavach, StarterTarget, StepRunner } from "@kavachlabs/core";

export const ErrRecordNotCommitted = new Error("kavach-kafkajs: record not committed");

export interface KafkaMessageLike {
  key?: Buffer | string | null;
  value: Buffer | string | null;
  timestamp?: string;
  offset?: string;
  headers?: Record<string, Buffer | string | (Buffer | string)[] | undefined>;
}

export interface ProducerRecordLike {
  topic: string;
  messages: KafkaMessageLike[];
  acks?: number;
  timeout?: number;
}

export interface ProducerBatchLike {
  topicMessages: ProducerRecordLike[];
  acks?: number;
  timeout?: number;
}

export interface RecordMetadataLike {
  topicName: string;
  partition: number;
  errorCode: number;
  baseOffset?: string;
}

export interface KafkaProducerLike {
  connect(): Promise<void>;
  disconnect(): Promise<void>;
  send(record: ProducerRecordLike): Promise<RecordMetadataLike[]>;
  sendBatch?(batch: ProducerBatchLike): Promise<RecordMetadataLike[]>;
}

export interface EachMessagePayloadLike {
  topic: string;
  partition: number;
  message: {
    key: Buffer | null;
    value: Buffer | null;
    timestamp: string;
    attributes: number;
    offset: string;
    headers?: Record<string, Buffer | string | undefined>;
  };
  heartbeat: () => Promise<void>;
  pause: () => () => void;
}

export interface EachBatchPayloadLike {
  batch: {
    topic: string;
    partition: number;
    highWatermark: string;
    messages: Array<EachMessagePayloadLike["message"]>;
  };
  resolveOffset: (offset: string) => void;
  heartbeat: () => Promise<void>;
  commitOffsetsIfNecessary: () => Promise<void>;
  uncommittedOffsets: () => Record<string, unknown>;
  isRunning: () => boolean;
  isStale: () => boolean;
}

export interface ConsumerRunConfigLike {
  autoCommit?: boolean;
  eachMessage?: (payload: EachMessagePayloadLike) => Promise<void>;
  eachBatch?: (payload: EachBatchPayloadLike) => Promise<void>;
}

export interface ConsumerSubscribeConfigLike {
  topic?: string | RegExp;
  topics?: Array<string | RegExp>;
  fromBeginning?: boolean;
}

export interface KafkaConsumerLike {
  connect(): Promise<void>;
  disconnect(): Promise<void>;
  subscribe(config: ConsumerSubscribeConfigLike): Promise<void>;
  run(config: ConsumerRunConfigLike): Promise<void>;
  commitOffsets?: (offsets: Array<{ topic: string; partition: number; offset: string }>) => Promise<void>;
}

export interface KafkaLike {
  producer(config?: Record<string, unknown>): KafkaProducerLike;
  consumer(config?: Record<string, unknown>): KafkaConsumerLike;
}

export interface WrapKafkaOptions {
  /** Never dial any real Kafka broker even in Record mode (ideal for local `kavach dev` capture). */
  offline?: boolean;
}

export interface WrapConsumerOptions extends WrapKafkaOptions {
  /** Default topic name when reconstructing Kafka messages from replay Input. */
  defaultTopic?: string;
}

export interface WrappedKafkaProducer<T extends KafkaProducerLike = KafkaProducerLike> extends KafkaProducerLike {
  raw(): T | null;
}

export interface WrappedKafkaConsumer<T extends KafkaConsumerLike = KafkaConsumerLike>
  extends KafkaConsumerLike,
    StepRunner,
    StarterTarget {
  raw(): T | null;
  /** Feed one message directly in local Record/test mode without a live Kafka broker. */
  handleMessage(topic: string, partition: number, message: KafkaMessageLike): Promise<void>;
  runStep(input: Input): Promise<void>;
  start(): Promise<void>;
}

export interface WrappedKafka<T extends KafkaLike = KafkaLike> extends KafkaLike {
  raw(): T | null;
  producer(config?: Record<string, unknown>): WrappedKafkaProducer;
  consumer(config?: Record<string, unknown>): WrappedKafkaConsumer;
}

/**
 * Converts a consumed KafkaJS message into a Kavach step `Input`:
 * `source: "kafka:<topic>"`, `position: "<partition>:<offset>"`, `data: raw value bytes`.
 */
export function recordToInput(topic: string, partition: number, message: KafkaMessageLike): Input {
  const headers: Record<string, string> = {};
  if (message.headers) {
    for (const [k, v] of Object.entries(message.headers)) {
      if (v === undefined) continue;
      if (Array.isArray(v)) {
        headers[k] = v.map((item) => (Buffer.isBuffer(item) ? item.toString("utf8") : String(item))).join(", ");
      } else {
        headers[k] = Buffer.isBuffer(v) ? v.toString("utf8") : String(v);
      }
    }
  }
  const keyBuf = toBufferOrNull(message.key);
  const valBuf = toBufferOrNull(message.value);
  return {
    source: `kafka:${topic}`,
    position: `${partition}:${message.offset ?? "0"}`,
    key: keyBuf ? new Uint8Array(keyBuf) : undefined,
    headers: Object.keys(headers).length > 0 ? headers : undefined,
    data: valBuf ? new Uint8Array(valBuf) : new Uint8Array(0),
  };
}

/**
 * Reconstructs a KafkaJS `EachMessagePayloadLike` from a Kavach replay `Input`.
 */
export function inputToMessage(input: Input, defaultTopic = ""): EachMessagePayloadLike {
  let topic = defaultTopic;
  if (input.source.startsWith("kafka:")) {
    const t = input.source.slice("kafka:".length);
    if (t) topic = t;
  }
  let partition = 0;
  let offset = "0";
  if (input.position) {
    const colon = input.position.indexOf(":");
    if (colon >= 0) {
      const p = Number(input.position.slice(0, colon));
      if (Number.isInteger(p)) partition = p;
      offset = input.position.slice(colon + 1) || "0";
    } else {
      offset = input.position;
    }
  }
  const headers: Record<string, Buffer> | undefined = input.headers
    ? Object.fromEntries(Object.entries(input.headers).map(([k, v]) => [k, Buffer.from(v, "utf8")]))
    : undefined;
  return {
    topic,
    partition,
    message: {
      key: input.key && input.key.length > 0 ? Buffer.from(input.key) : null,
      value: Buffer.from(input.data),
      timestamp: "0",
      attributes: 0,
      offset,
      headers,
    },
    heartbeat: async () => {},
    pause: () => () => {},
  };
}

/**
 * Wraps a KafkaJS `Producer` (or `null` in Replay/offline mode).
 * Captures every produced message as an `Output` on sink `"kafka:<topic>"` with raw JSON
 * payload bytes preserved so `.kavach/invariants.yaml` expressions like `$.transaction_id`
 * match out of the box. Never dials a broker in Replay mode.
 */
export function wrapProducer<T extends KafkaProducerLike>(
  kv: Kavach,
  producer: T | null,
  opts: WrapKafkaOptions = {},
): WrappedKafkaProducer<T> {
  const isOffline = () => Boolean(opts.offline) || kv.isReplay();

  const recordMessages = (topic: string, messages: KafkaMessageLike[]) => {
    if (kv.isOff()) return;
    for (const msg of messages) {
      const keyStr = msg.key !== undefined && msg.key !== null ? toBufferOrNull(msg.key)!.toString("utf8") : "";
      kv.recordOutput({
        sink: `kafka:${topic}`,
        key: keyStr,
        data: encodeKafkaOutputData(topic, msg),
      });
    }
  };

  return {
    raw: () => producer,
    async connect(): Promise<void> {
      if (isOffline() || !producer) return;
      await producer.connect();
    },
    async disconnect(): Promise<void> {
      if (isOffline() || !producer) return;
      await producer.disconnect();
    },
    async send(record: ProducerRecordLike): Promise<RecordMetadataLike[]> {
      if (isOffline()) {
        recordMessages(record.topic, record.messages);
        return [
          {
            topicName: record.topic,
            partition: 0,
            errorCode: 0,
            baseOffset: "0",
          },
        ];
      }
      if (!producer) throw new Error("kavach-kafkajs: nil underlying KafkaJS producer");
      const meta = await producer.send(record);
      recordMessages(record.topic, record.messages);
      return meta;
    },
    async sendBatch(batch: ProducerBatchLike): Promise<RecordMetadataLike[]> {
      if (isOffline()) {
        const out: RecordMetadataLike[] = [];
        for (const tm of batch.topicMessages) {
          recordMessages(tm.topic, tm.messages);
          out.push({ topicName: tm.topic, partition: 0, errorCode: 0, baseOffset: "0" });
        }
        return out;
      }
      if (!producer) throw new Error("kavach-kafkajs: nil underlying KafkaJS producer");
      if (producer.sendBatch) {
        const meta = await producer.sendBatch(batch);
        for (const tm of batch.topicMessages) {
          recordMessages(tm.topic, tm.messages);
        }
        return meta;
      }
      const out: RecordMetadataLike[] = [];
      for (const tm of batch.topicMessages) {
        const m = await producer.send(tm);
        recordMessages(tm.topic, tm.messages);
        out.push(...m);
      }
      return out;
    },
  };
}

/**
 * Wraps a KafkaJS `Consumer` (or `null` in Replay/offline mode).
 * Captures `kafka:<topic>` step inputs, signals retryable errors on thrown or uncommitted
 * messages, and implements `StepRunner` so `kv.serveOrReplay(consumer)` replays deterministically
 * with zero Kafka broker socket.
 */
export function wrapConsumer<T extends KafkaConsumerLike>(
  kv: Kavach,
  consumer: T | null,
  opts: WrapConsumerOptions = {},
): WrappedKafkaConsumer<T> {
  const isOffline = () => Boolean(opts.offline) || kv.isReplay();
  let defaultTopic = opts.defaultTopic ?? "";
  let runConfig: ConsumerRunConfigLike | undefined;

  const invokeHandler = async (payload: EachMessagePayloadLike): Promise<void> => {
    if (!runConfig) return;
    if (runConfig.eachMessage) {
      await runConfig.eachMessage(payload);
      return;
    }
    if (runConfig.eachBatch) {
      let resolvedOffset = false;
      const requireCommit = runConfig.autoCommit === false;
      await runConfig.eachBatch({
        batch: {
          topic: payload.topic,
          partition: payload.partition,
          highWatermark: String(Number(payload.message.offset || "0") + 1),
          messages: [payload.message],
        },
        resolveOffset: () => {
          resolvedOffset = true;
        },
        heartbeat: payload.heartbeat,
        commitOffsetsIfNecessary: async () => {},
        uncommittedOffsets: () => ({}),
        isRunning: () => true,
        isStale: () => false,
      });
      if (requireCommit && !resolvedOffset) {
        throw ErrRecordNotCommitted;
      }
    }
  };

  const wrapped: WrappedKafkaConsumer<T> = {
    raw: () => consumer,
    async connect(): Promise<void> {
      if (isOffline() || !consumer) return;
      await consumer.connect();
    },
    async disconnect(): Promise<void> {
      if (isOffline() || !consumer) return;
      await consumer.disconnect();
    },
    async subscribe(config: ConsumerSubscribeConfigLike): Promise<void> {
      if (typeof config.topic === "string" && config.topic) {
        defaultTopic = config.topic;
      } else if (Array.isArray(config.topics)) {
        const firstStr = config.topics.find((t): t is string => typeof t === "string" && t.length > 0);
        if (firstStr) defaultTopic = firstStr;
      }
      if (isOffline() || !consumer) return;
      await consumer.subscribe(config);
    },
    async run(config: ConsumerRunConfigLike): Promise<void> {
      runConfig = config;
      if (kv.isReplay()) {
        return;
      }
      if (opts.offline || !consumer) {
        return;
      }
      await consumer.run({
        autoCommit: config.autoCommit,
        eachMessage: config.eachMessage
          ? async (payload) => {
              if (!kv.isRecord()) {
                await config.eachMessage!(payload);
                return;
              }
              const input = recordToInput(payload.topic, payload.partition, payload.message);
              await kv.runStep(input, async () => {
                await config.eachMessage!(payload);
              });
            }
          : undefined,
        eachBatch: config.eachBatch
          ? async (batchPayload) => {
              if (!kv.isRecord()) {
                await config.eachBatch!(batchPayload);
                return;
              }
              for (const msg of batchPayload.batch.messages) {
                const input = recordToInput(batchPayload.batch.topic, batchPayload.batch.partition, msg);
                await kv.runStep(input, async () => {
                  let resolved = false;
                  await config.eachBatch!({
                    ...batchPayload,
                    batch: { ...batchPayload.batch, messages: [msg] },
                    resolveOffset: (off) => {
                      resolved = true;
                      batchPayload.resolveOffset(off);
                    },
                  });
                  if (config.autoCommit === false && !resolved) {
                    throw ErrRecordNotCommitted;
                  }
                });
              }
            }
          : undefined,
      });
    },
    async commitOffsets(offsets: Array<{ topic: string; partition: number; offset: string }>): Promise<void> {
      if (isOffline() || !consumer?.commitOffsets) return;
      await consumer.commitOffsets(offsets);
    },
    async handleMessage(topic: string, partition: number, message: KafkaMessageLike): Promise<void> {
      const input = recordToInput(topic, partition, message);
      const payload = inputToMessage(input, topic || defaultTopic);
      if (kv.isRecord()) {
        await kv.runStep(input, async () => {
          await invokeHandler(payload);
        });
      } else {
        await invokeHandler(payload);
      }
    },
    async runStep(input: Input): Promise<void> {
      const payload = inputToMessage(input, defaultTopic);
      await invokeHandler(payload);
    },
    async start(): Promise<void> {
      if (kv.isReplay()) {
        await kv.serveHost(wrapped);
      }
    },
  };

  return wrapped;
}

/**
 * Wraps a KafkaJS `Kafka` instance (or `null` in Replay/offline mode) so `.producer()`
 * and `.consumer()` automatically return Kavach-wrapped clients with zero broker socket in Replay.
 */
export function wrapKafka<T extends KafkaLike>(
  kv: Kavach,
  kafka: T | null,
  opts: WrapKafkaOptions = {},
): WrappedKafka<T> {
  const isOffline = () => Boolean(opts.offline) || kv.isReplay();
  return {
    raw: () => kafka,
    producer(config?: Record<string, unknown>): WrappedKafkaProducer {
      const raw = !isOffline() && kafka ? kafka.producer(config) : null;
      return wrapProducer(kv, raw, opts);
    },
    consumer(config?: Record<string, unknown>): WrappedKafkaConsumer {
      const raw = !isOffline() && kafka ? kafka.consumer(config) : null;
      return wrapConsumer(kv, raw, opts);
    },
  };
}

/** Creates an offline Replay KafkaJS client that never opens a broker socket. */
export function createReplayKafka(kv: Kavach, opts: WrapKafkaOptions = {}): WrappedKafka<KafkaLike> {
  return wrapKafka<KafkaLike>(kv, null, { ...opts, offline: true });
}

/**
 * Returns compact raw JSON payload bytes directly when `msg.value` is valid JSON
 * (so declarative `.kavach/invariants.yaml` JSONPath checks like `$.transaction_id`
 * match out of the box), or wraps non-JSON payloads in a structured JSON envelope.
 */
function encodeKafkaOutputData(topic: string, msg: KafkaMessageLike): Uint8Array {
  const valBuf = toBufferOrNull(msg.value);
  if (valBuf && valBuf.length > 0) {
    const trimmed = valBuf.toString("utf8").trim();
    if (trimmed.length > 0 && (trimmed[0] === "{" || trimmed[0] === "[")) {
      try {
        const parsed: unknown = JSON.parse(trimmed);
        return new Uint8Array(Buffer.from(JSON.stringify(parsed), "utf8"));
      } catch {
        // fall through to structured envelope
      }
    }
  }
  const keyBuf = toBufferOrNull(msg.key);
  const env: Record<string, unknown> = { topic };
  if (keyBuf && keyBuf.length > 0) env.key = keyBuf.toString("utf8");
  if (valBuf && valBuf.length > 0) env.value = valBuf.toString("base64");
  if (msg.headers) {
    const hdrs: Record<string, string> = {};
    for (const [k, v] of Object.entries(msg.headers)) {
      if (v === undefined) continue;
      hdrs[k] = Array.isArray(v)
        ? v.map((x) => (Buffer.isBuffer(x) ? x.toString("utf8") : String(x))).join(", ")
        : Buffer.isBuffer(v)
          ? v.toString("utf8")
          : String(v);
    }
    if (Object.keys(hdrs).length > 0) env.headers = hdrs;
  }
  return new Uint8Array(Buffer.from(JSON.stringify(env), "utf8"));
}

function toBufferOrNull(v: Buffer | string | null | undefined): Buffer | null {
  if (v === null || v === undefined) return null;
  return Buffer.isBuffer(v) ? v : Buffer.from(v, "utf8");
}
