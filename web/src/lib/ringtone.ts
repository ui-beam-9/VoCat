// 来电振铃提示音：预设铃声为 Web Audio 合成，自定义铃声播放用户上传的音频文件。
// 浏览器 autoplay 限制：AudioContext/HTMLAudio 必须在用户手势后创建/resume/play，
// 由 unlockRingtoneAudio() 在页面首次交互（点击/按键）时完成。

export type RingtoneChoice = "classic" | "single" | "chime" | "beep" | "custom";

export const DEFAULT_RINGTONE = "classic";

// 自定义铃声音频流地址（后端按上传时的 MIME 返回）。
export const CUSTOM_RINGTONE_URL = "/api/settings/ringtone/custom";

interface ToneSegment {
  frequencies: number[];
  durationMs: number;
}

interface RingtonePattern {
  segments: ToneSegment[];
  // 一轮播完后的停顿时长。
  pauseMs: number;
  volume: number;
}

const RINGTONE_PATTERNS: Record<Exclude<RingtoneChoice, "custom">, RingtonePattern> = {
  // 经典双音（440+480Hz 标准振铃音）：响 2s 停 4s。
  classic: {
    segments: [{ frequencies: [440, 480], durationMs: 2000 }],
    pauseMs: 4000,
    volume: 0.12,
  },
  // 单音（450Hz）：响 1s 停 4s。
  single: {
    segments: [{ frequencies: [450], durationMs: 1000 }],
    pauseMs: 4000,
    volume: 0.14,
  },
  // 上升琶音：523/659/784/1047Hz 各 0.12s，停 3s。
  chime: {
    segments: [
      { frequencies: [523.25], durationMs: 120 },
      { frequencies: [659.25], durationMs: 120 },
      { frequencies: [783.99], durationMs: 120 },
      { frequencies: [1046.5], durationMs: 240 },
    ],
    pauseMs: 3000,
    volume: 0.16,
  },
  // 急促蜂鸣：1200Hz 0.2s × 2（间隔 0.2s），停 2s。
  beep: {
    segments: [
      { frequencies: [1200], durationMs: 200 },
      { frequencies: [], durationMs: 200 },
      { frequencies: [1200], durationMs: 200 },
    ],
    pauseMs: 2000,
    volume: 0.14,
  },
};

const PREVIEW_DURATION_MS = 4000;

let sharedContext: AudioContext | null = null;
let activeOscillators: OscillatorNode[] = [];
let activeGain: GainNode | null = null;
let cycleTimer: number | undefined;
let previewTimer: number | undefined;
let ringing = false;
let currentChoice: RingtoneChoice = DEFAULT_RINGTONE;
let customAudio: HTMLAudioElement | null = null;

function audioContextCtor(): (new () => AudioContext) | undefined {
  return (
    window.AudioContext ??
    (window as unknown as { webkitAudioContext?: new () => AudioContext }).webkitAudioContext
  );
}

function ensureContext(): AudioContext | null {
  if (sharedContext) return sharedContext;
  const Ctor = audioContextCtor();
  if (!Ctor) return null;
  try {
    sharedContext = new Ctor();
  } catch {
    return null;
  }
  return sharedContext;
}

function ensureCustomAudio(): HTMLAudioElement | null {
  if (customAudio) return customAudio;
  try {
    customAudio = new Audio(CUSTOM_RINGTONE_URL);
    customAudio.loop = true;
    customAudio.preload = "auto";
  } catch {
    return null;
  }
  return customAudio;
}

// 在用户手势中调用：创建并恢复 AudioContext、解锁 Audio 元素，之后来电铃声才能发声。
export function unlockRingtoneAudio(): void {
  const ctx = ensureContext();
  if (ctx && ctx.state === "suspended") void ctx.resume().catch(() => {});
  const audio = ensureCustomAudio();
  if (audio) {
    // 静音播放一次立即暂停，完成 HTMLAudio 的 autoplay 解锁。
    audio.muted = true;
    audio
      .play()
      .then(() => {
        audio.pause();
        audio.currentTime = 0;
        audio.muted = false;
      })
      .catch(() => {
        audio.muted = false;
      });
  }
}

// 重新上传自定义铃声后调用：丢弃缓存的 Audio 元素，下次播放时重新拉取。
export function invalidateCustomRingtone(): void {
  if (customAudio) {
    customAudio.pause();
    customAudio = null;
  }
}

// 设置当前来电铃声来源；custom 无已上传文件时由播放路径回退到默认预设。
export function configureRingtone(choice: string): void {
  currentChoice = normalizeChoice(choice);
}

function normalizeChoice(choice: string): RingtoneChoice {
  if (choice === "single" || choice === "chime" || choice === "beep" || choice === "custom") {
    return choice;
  }
  return DEFAULT_RINGTONE;
}

function stopBurst(): void {
  for (const osc of activeOscillators) {
    try {
      osc.stop();
    } catch {
      /* 已停止 */
    }
    osc.disconnect();
  }
  activeOscillators = [];
  activeGain?.disconnect();
  activeGain = null;
}

// 播放一轮预设 pattern（多段按时序排布的短音），返回整轮时长（含停顿）。
function playPresetCycle(ctx: AudioContext, pattern: RingtonePattern): number {
  stopBurst();
  const gain = ctx.createGain();
  gain.connect(ctx.destination);
  const start = ctx.currentTime;
  let offsetMs = 0;
  for (const segment of pattern.segments) {
    const onSeconds = segment.durationMs / 1000;
    const segmentStart = start + offsetMs / 1000;
    if (segment.frequencies.length > 0) {
      // 每段淡入淡出，避免爆音。
      gain.gain.setValueAtTime(0.0001, segmentStart);
      gain.gain.exponentialRampToValueAtTime(pattern.volume, segmentStart + 0.02);
      gain.gain.setValueAtTime(pattern.volume, segmentStart + Math.max(0.02, onSeconds - 0.04));
      gain.gain.exponentialRampToValueAtTime(0.0001, segmentStart + onSeconds);
      for (const frequency of segment.frequencies) {
        const osc = ctx.createOscillator();
        osc.type = "sine";
        osc.frequency.value = frequency;
        osc.connect(gain);
        osc.start(segmentStart);
        osc.stop(segmentStart + onSeconds);
        activeOscillators.push(osc);
      }
    }
    offsetMs += segment.durationMs;
  }
  activeGain = gain;
  return offsetMs + pattern.pauseMs;
}

function startPresetLoop(choice: Exclude<RingtoneChoice, "custom">): void {
  const ctx = ensureContext();
  if (!ctx) return;
  if (ctx.state === "suspended") void ctx.resume().catch(() => {});
  const pattern = RINGTONE_PATTERNS[choice];
  const cycleMs = playPresetCycle(ctx, pattern);
  cycleTimer = window.setInterval(() => {
    if (!sharedContext) return;
    playPresetCycle(sharedContext, pattern);
  }, cycleMs);
}

function startCustomLoop(): boolean {
  const audio = ensureCustomAudio();
  if (!audio) return false;
  audio.currentTime = 0;
  audio
    .play()
    .catch(() => {
      /* 未解锁或文件缺失时静默 */
    });
  return true;
}

// 开始循环振铃；重复调用幂等。
export function startRingtone(): void {
  if (ringing) return;
  ringing = true;
  if (currentChoice === "custom") {
    if (startCustomLoop()) return;
    // 自定义音频不可用（未上传/浏览器不支持）时回退默认预设。
    startPresetLoop(DEFAULT_RINGTONE);
    return;
  }
  startPresetLoop(currentChoice);
}

// 停止振铃/试听并释放振荡器与音频；AudioContext 保留复用。
export function stopRingtone(): void {
  ringing = false;
  if (cycleTimer !== undefined) {
    window.clearInterval(cycleTimer);
    cycleTimer = undefined;
  }
  if (previewTimer !== undefined) {
    window.clearTimeout(previewTimer);
    previewTimer = undefined;
  }
  stopBurst();
  if (customAudio) {
    customAudio.pause();
    try {
      customAudio.currentTime = 0;
    } catch {
      /* 元数据未加载时忽略 */
    }
  }
}

// 试听指定铃声：播放约 4 秒后自动停止。
export function previewRingtone(choice: string): void {
  stopRingtone();
  const normalized = normalizeChoice(choice);
  if (normalized === "custom") {
    startCustomLoop();
  } else {
    startPresetLoop(normalized);
  }
  previewTimer = window.setTimeout(() => stopRingtone(), PREVIEW_DURATION_MS);
}
