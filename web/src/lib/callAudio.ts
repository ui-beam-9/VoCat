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

// Worklet 处理代码是同源静态文件（web/public/call-worklet.js），随构建产物发布。
// 必须同源加载：服务端 CSP 为 script-src 'self'，用 Blob URL 会被拦截导致
// "Failed to load worklet module script"。
const WORKLET_URL = "/call-worklet.js";

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
export function mediaErrorMessage(error: unknown): string {
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

// 语音通道 WebSocket 升级失败时，用普通 HTTP GET 回查服务端返回的 JSON 错误，
// 以便把真实原因（如 call_media_unavailable / 通话状态不满足）展示给用户。
async function probeMediaError(deviceId: string, callId: string): Promise<string> {
  try {
    const httpURL = callMediaURL(deviceId, callId).replace(/^ws/, "http");
    const response = await fetch(httpURL, {
      credentials: "include",
      headers: { Accept: "application/json" },
      cache: "no-store",
    });
    const text = await response.text();
    if (!text) return "";
    try {
      const payload = JSON.parse(text) as { error?: { message?: string }; message?: string };
      return payload.error?.message || payload.message || "";
    } catch {
      return text.slice(0, 200);
    }
  } catch {
    return "";
  }
}

export function callMediaURL(deviceId: string, callId: string): string {
  const scheme = window.location.protocol === "https:" ? "wss" : "ws";
  const params = new URLSearchParams({ call_id: callId });
  return `${scheme}://${window.location.host}/api/devices/${encodeURIComponent(deviceId)}/calls/media?${params.toString()}`;
}

// 加载同源 worklet 模块。
async function loadWorkletModule(ctx: AudioContext): Promise<void> {
  await ctx.audioWorklet.addModule(WORKLET_URL);
}

function audioContextCtor(): (new (options?: AudioContextOptions) => AudioContext) | undefined {
  return (
    window.AudioContext ??
    (window as unknown as { webkitAudioContext?: new (options?: AudioContextOptions) => AudioContext }).webkitAudioContext
  );
}

// 把播放输出路由到指定设备（AudioContext.setSinkId，Chrome 110+）。
// 浏览器不支持或设备不可用时静默忽略，保持默认输出。
async function applySinkId(ctx: AudioContext, sinkId?: string): Promise<void> {
  if (!sinkId) return;
  const target = ctx as AudioContext & { setSinkId?: (id: string) => Promise<void> };
  if (typeof target.setSinkId !== "function") return;
  try {
    await target.setSinkId(sinkId);
  } catch {
    /* 忽略：浏览器或系统未提供该输出设备 */
  }
}

// 是否支持在网页里选择播放设备。
export function audioOutputSelectionSupported(): boolean {
  if (typeof window === "undefined") return false;
  const Ctor = audioContextCtor();
  return !!Ctor && typeof (Ctor.prototype as { setSinkId?: unknown }).setSinkId === "function";
}

export interface AudioDeviceInfo {
  deviceId: string;
  label: string;
}

export interface AudioDeviceLists {
  inputs: AudioDeviceInfo[];
  outputs: AudioDeviceInfo[];
}

// 枚举音频输入/输出设备。未授权时 label 可能为空字符串。
export async function listAudioDevices(): Promise<AudioDeviceLists> {
  if (typeof navigator === "undefined" || !navigator.mediaDevices?.enumerateDevices) {
    return { inputs: [], outputs: [] };
  }
  const devices = await navigator.mediaDevices.enumerateDevices();
  const inputs: AudioDeviceInfo[] = [];
  const outputs: AudioDeviceInfo[] = [];
  for (const device of devices) {
    if (device.kind === "audioinput") {
      inputs.push({ deviceId: device.deviceId, label: device.label });
    } else if (device.kind === "audiooutput") {
      outputs.push({ deviceId: device.deviceId, label: device.label });
    }
  }
  return { inputs, outputs };
}

// 播放一段测试音（880Hz 短音），走所选播放设备。
export async function playTestTone(sinkId?: string): Promise<void> {
  const Ctor = audioContextCtor();
  if (!Ctor) throw new Error("当前浏览器不支持 Web Audio");
  const ctx = new Ctor();
  try {
    await applySinkId(ctx, sinkId);
    if (ctx.state === "suspended") await ctx.resume();
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = "sine";
    osc.frequency.value = 880;
    osc.connect(gain);
    gain.connect(ctx.destination);
    const start = ctx.currentTime;
    // 淡入淡出，避免爆音。
    gain.gain.setValueAtTime(0.0001, start);
    gain.gain.exponentialRampToValueAtTime(0.25, start + 0.05);
    gain.gain.setValueAtTime(0.25, start + 0.45);
    gain.gain.exponentialRampToValueAtTime(0.0001, start + 0.55);
    osc.start(start);
    osc.stop(start + 0.6);
    await new Promise((resolve) => setTimeout(resolve, 700));
  } finally {
    if (ctx.state !== "closed") void ctx.close();
  }
}

export interface MicLevelOptions {
  deviceId?: string;
  durationMs?: number;
  threshold?: number;
  onLevel?: (level: number) => void;
}

export interface MicLevelResult {
  detected: boolean;
  peak: number;
}

// 采集麦克风电平：在 durationMs 内持续测量 RMS，任一时刻超过阈值即视为"检测到说话声"。
export async function measureMicLevel(options: MicLevelOptions = {}): Promise<MicLevelResult> {
  const { deviceId, durationMs = 4000, threshold = 0.015, onLevel } = options;
  if (typeof navigator === "undefined" || !navigator.mediaDevices?.getUserMedia) {
    throw new Error(callAudioUnsupportedReason() || "浏览器不支持麦克风采集");
  }
  const audio: MediaTrackConstraints = {
    echoCancellation: true,
    noiseSuppression: true,
    autoGainControl: true,
  };
  if (deviceId) audio.deviceId = { exact: deviceId };
  const stream = await navigator.mediaDevices.getUserMedia({ audio });
  const Ctor = audioContextCtor();
  if (!Ctor) {
    stream.getTracks().forEach((track) => track.stop());
    throw new Error("当前浏览器不支持 Web Audio");
  }
  const ctx = new Ctor();
  try {
    if (ctx.state === "suspended") await ctx.resume();
    const source = ctx.createMediaStreamSource(stream);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 2048;
    source.connect(analyser);
    const buffer = new Float32Array(analyser.fftSize);
    const deadline = performance.now() + durationMs;
    let peak = 0;
    let detected = false;
    while (performance.now() < deadline) {
      analyser.getFloatTimeDomainData(buffer);
      let sum = 0;
      for (let index = 0; index < buffer.length; index++) {
        sum += buffer[index] * buffer[index];
      }
      const rms = Math.sqrt(sum / buffer.length);
      if (rms > peak) peak = rms;
      onLevel?.(rms);
      if (rms >= threshold) detected = true;
      await new Promise((resolve) => setTimeout(resolve, 80));
    }
    return { detected, peak };
  } finally {
    stream.getTracks().forEach((track) => track.stop());
    if (ctx.state !== "closed") void ctx.close();
  }
}

export interface CallAudioRouteOptions {
  inputDeviceId?: string;
  outputDeviceId?: string;
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
  private wsFailed = false;

  constructor(
    private readonly deviceId: string,
    private readonly callId: string,
    private readonly events: CallAudioEvents = {},
    private readonly route: CallAudioRouteOptions = {},
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
      const audio: MediaTrackConstraints = {
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      };
      if (this.route.inputDeviceId) audio.deviceId = { exact: this.route.inputDeviceId };
      this.stream = await navigator.mediaDevices.getUserMedia({ audio });

      const Ctor = audioContextCtor();
      if (!Ctor) throw new Error("当前浏览器不支持 Web Audio");
      let ctx: AudioContext;
      try {
        ctx = new Ctor({ sampleRate: TARGET_SAMPLE_RATE });
      } catch {
        // 部分浏览器不支持指定采样率，回退到默认采样率，由 Worklet 负责换算。
        ctx = new Ctor();
      }
      this.ctx = ctx;
      await applySinkId(ctx, this.route.outputDeviceId);
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
      ws.onerror = () => {
        this.wsFailed = true;
      };
      ws.onclose = (event: CloseEvent) => {
        const closedByUs = this.closed;
        this.stop();
        if (closedByUs || !this.wsFailed) return;
        void probeMediaError(this.deviceId, this.callId).then((detail) => {
          this.events.onState?.(
            "error",
            detail || `语音通道连接失败（WebSocket ${event.code || 1006}）`,
          );
        });
      };
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
