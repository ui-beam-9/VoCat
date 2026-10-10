// 通话音频桥：把后端 WebSocket 的 PCM 流与浏览器麦克风/扬声器对接。
//
// 契约（见 internal/server/call_media_api.go）：
// - 每条 WebSocket 二进制消息 = 小端有符号 16 位、8kHz、单声道 PCM
// - 双向：收到的消息播放，发送的消息作为上行送入 IMS
//
// 通过 AudioWorklet 完成采样率换算，因此在 48kHz 等宿主采样率下同样工作。

export type CallAudioState = "connecting" | "live" | "closed" | "error";

export interface CallAudioEvents {
  onState?: (state: CallAudioState, detail?: string) => void;
}

const TARGET_SAMPLE_RATE = 8000;
const FRAME_SAMPLES = 160; // 20ms @ 8kHz

// AudioWorklet 模块以 Blob URL 注入，避免额外的构建资源与路由。
const WORKLET_SOURCE = `
class CaptureProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ratio = sampleRate / ${TARGET_SAMPLE_RATE};
    this.inBuf = new Float32Array(0);
    this.pos = 0;
    this.frame = new Float32Array(${FRAME_SAMPLES});
    this.frameLen = 0;
  }
  drain() {
    while (this.pos + 1 < this.inBuf.length) {
      const i = Math.floor(this.pos);
      const frac = this.pos - i;
      this.frame[this.frameLen++] = this.inBuf[i] * (1 - frac) + this.inBuf[i + 1] * frac;
      this.pos += this.ratio;
      if (this.frameLen === ${FRAME_SAMPLES}) {
        const pcm = new Int16Array(${FRAME_SAMPLES});
        for (let k = 0; k < pcm.length; k++) {
          const v = Math.max(-1, Math.min(1, this.frame[k]));
          pcm[k] = v < 0 ? Math.round(v * 0x8000) : Math.round(v * 0x7fff);
        }
        this.port.postMessage(pcm.buffer, [pcm.buffer]);
        this.frameLen = 0;
      }
    }
    const consumed = Math.floor(this.pos);
    if (consumed > 0) {
      this.inBuf = this.inBuf.slice(consumed);
      this.pos -= consumed;
    }
  }
  process(inputs) {
    const input = inputs[0];
    if (input && input[0]) {
      const ch = input[0];
      const merged = new Float32Array(this.inBuf.length + ch.length);
      merged.set(this.inBuf, 0);
      merged.set(ch, this.inBuf.length);
      this.inBuf = merged;
      this.drain();
    }
    return true;
  }
}
registerProcessor('vocat-capture', CaptureProcessor);

class PlaybackProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.step = ${TARGET_SAMPLE_RATE} / sampleRate;
    this.buf = new Float32Array(0);
    this.pos = 0;
    this.maxBuf = ${TARGET_SAMPLE_RATE} * 3;
    this.port.onmessage = (event) => {
      const src = new Int16Array(event.data);
      const f = new Float32Array(src.length);
      for (let i = 0; i < src.length; i++) f[i] = src[i] / 0x8000;
      const merged = new Float32Array(this.buf.length + f.length);
      merged.set(this.buf, 0);
      merged.set(f, this.buf.length);
      // 缓冲过大说明播放滞后，丢弃最旧数据以压低延迟。
      this.buf = merged.length > this.maxBuf ? merged.slice(merged.length - this.maxBuf) : merged;
      if (this.pos > this.buf.length) this.pos = 0;
    };
  }
  process(_inputs, outputs) {
    const out = outputs[0][0];
    if (!out) return true;
    for (let i = 0; i < out.length; i++) {
      if (this.pos + 1 < this.buf.length) {
        const j = Math.floor(this.pos);
        const frac = this.pos - j;
        out[i] = this.buf[j] * (1 - frac) + this.buf[j + 1] * frac;
        this.pos += this.step;
      } else {
        out[i] = 0;
      }
    }
    const consumed = Math.floor(this.pos);
    if (consumed > 0) {
      this.buf = this.buf.slice(consumed);
      this.pos -= consumed;
    }
    return true;
  }
}
registerProcessor('vocat-playback', PlaybackProcessor);
`;

// getUserMedia 只在安全上下文（HTTPS / localhost）暴露；HTTP 内网访问时
// navigator.mediaDevices 为 undefined，直接调用会抛出 "Cannot read properties of undefined"。
export function callAudioSupported(): boolean {
  return typeof navigator !== "undefined" && !!navigator.mediaDevices?.getUserMedia;
}

// 音频环境诊断：用于区分"内网 IP / HTTP 非安全上下文"与"浏览器不支持"等不同原因。
export type CallAudioIssue = "" | "insecure_context" | "no_media_devices" | "no_get_user_media";

export interface CallAudioDiagnostics {
  supported: boolean;
  secureContext: boolean;
  hasMediaDevices: boolean;
  hasGetUserMedia: boolean;
  origin: string;
  issue: CallAudioIssue;
}

export function callAudioDiagnostics(): CallAudioDiagnostics {
  const win = typeof window !== "undefined" ? window : undefined;
  const nav = typeof navigator !== "undefined" ? navigator : undefined;
  const secureContext = !!win?.isSecureContext;
  const hasMediaDevices = !!nav?.mediaDevices;
  const hasGetUserMedia = !!nav?.mediaDevices?.getUserMedia;
  const origin = win?.location?.origin ?? "";
  let issue: CallAudioIssue = "";
  if (!hasGetUserMedia) {
    if (!secureContext) issue = "insecure_context";
    else if (!hasMediaDevices) issue = "no_media_devices";
    else issue = "no_get_user_media";
  }
  return { supported: hasGetUserMedia, secureContext, hasMediaDevices, hasGetUserMedia, origin, issue };
}

export function callAudioUnsupportedReason(): string {
  const diag = callAudioDiagnostics();
  switch (diag.issue) {
    case "insecure_context":
      return `当前以 ${diag.origin || "http 地址"} 访问，不是安全上下文；浏览器只在 HTTPS 或 localhost 下提供麦克风/扬声器`;
    case "no_media_devices":
      return "浏览器未提供 navigator.mediaDevices 接口";
    case "no_get_user_media":
      return "浏览器未提供 getUserMedia 接口";
    default:
      return "";
  }
}

// 把浏览器的媒体错误翻译为可读提示。
function mediaErrorMessage(error: unknown): string {
  if (typeof DOMException !== "undefined" && error instanceof DOMException) {
    switch (error.name) {
      case "NotAllowedError":
      case "SecurityError":
        return "麦克风权限被拒绝或被浏览器策略阻止，请允许麦克风后重试";
      case "NotFoundError":
        return "未找到可用的麦克风设备";
      case "NotReadableError":
        return "麦克风被其他程序占用，无法读取";
      case "OverconstrainedError":
        return "麦克风不支持所需的音频参数";
      default:
        return error.message || error.name;
    }
  }
  return error instanceof Error ? error.message : String(error);
}

export function callMediaURL(deviceId: string, callId: string): string {
  const scheme = window.location.protocol === "https:" ? "wss" : "ws";
  const params = new URLSearchParams({ call_id: callId });
  return `${scheme}://${window.location.host}/api/devices/${encodeURIComponent(deviceId)}/calls/media?${params.toString()}`;
}

async function loadWorkletModule(ctx: AudioContext): Promise<void> {
  const blob = new Blob([WORKLET_SOURCE], { type: "application/javascript" });
  const url = URL.createObjectURL(blob);
  try {
    await ctx.audioWorklet.addModule(url);
  } finally {
    URL.revokeObjectURL(url);
  }
}

// 一次通话对应的音频会话：麦克风采集 + 扬声器播放 + WebSocket 传输。
export class CallAudioSession {
  private ws?: WebSocket;
  private ctx?: AudioContext;
  private stream?: MediaStream;
  private source?: MediaStreamAudioSourceNode;
  private captureNode?: AudioWorkletNode;
  private playbackNode?: AudioWorkletNode;
  private closed = false;

  constructor(
    private readonly deviceId: string,
    private readonly callId: string,
    private readonly events: CallAudioEvents = {},
  ) {}

  async start(): Promise<void> {
    this.events.onState?.("connecting");
    if (!callAudioSupported()) {
      const reason = callAudioUnsupportedReason();
      this.events.onState?.("error", reason);
      throw new Error(reason);
    }
    try {
      // 先申请麦克风权限，便于尽早抛出明确的授权错误。
      this.stream = await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
      });

      const Ctor = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
      if (!Ctor) throw new Error("当前浏览器不支持 Web Audio");
      let ctx: AudioContext;
      try {
        ctx = new Ctor({ sampleRate: TARGET_SAMPLE_RATE });
      } catch {
        // 部分浏览器不支持指定采样率，回退到默认采样率，由 Worklet 负责换算。
        ctx = new Ctor();
      }
      this.ctx = ctx;
      await loadWorkletModule(ctx);

      const capture = new AudioWorkletNode(ctx, "vocat-capture");
      const playback = new AudioWorkletNode(ctx, "vocat-playback", {
        numberOfInputs: 0,
        numberOfOutputs: 1,
        outputChannelCount: [1],
      });
      this.captureNode = capture;
      this.playbackNode = playback;

      this.source = ctx.createMediaStreamSource(this.stream);
      this.source.connect(capture);
      capture.port.onmessage = (event: MessageEvent<ArrayBuffer>) => {
        if (this.ws && this.ws.readyState === WebSocket.OPEN) {
          this.ws.send(event.data);
        }
      };
      playback.connect(ctx.destination);

      const ws = new WebSocket(callMediaURL(this.deviceId, this.callId));
      ws.binaryType = "arraybuffer";
      this.ws = ws;
      ws.onopen = () => {
        if (ctx.state === "suspended") void ctx.resume();
        this.events.onState?.("live");
      };
      ws.onmessage = (event: MessageEvent<ArrayBuffer | string>) => {
        if (typeof event.data === "string") return;
        try {
          this.playbackNode?.port.postMessage(event.data, [event.data]);
        } catch {
          this.playbackNode?.port.postMessage(event.data);
        }
      };
      ws.onerror = () => this.events.onState?.("error", "WebSocket error");
      ws.onclose = () => this.stop();
    } catch (error) {
      const message = mediaErrorMessage(error);
      this.stop();
      this.events.onState?.("error", message);
      throw new Error(message);
    }
  }

  stop(): void {
    if (this.closed) return;
    this.closed = true;
    try {
      this.ws?.close();
    } catch {
      /* ignore */
    }
    try {
      this.captureNode?.port.close();
    } catch {
      /* ignore */
    }
    try {
      this.source?.disconnect();
      this.captureNode?.disconnect();
      this.playbackNode?.disconnect();
    } catch {
      /* ignore */
    }
    this.stream?.getTracks().forEach((track) => track.stop());
    if (this.ctx && this.ctx.state !== "closed") void this.ctx.close();
    this.events.onState?.("closed");
  }
}
