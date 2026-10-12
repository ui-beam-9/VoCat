// 来电振铃提示音：Web Audio 合成双音（440+480Hz），响 2 秒停 4 秒循环。
// 浏览器 autoplay 限制：AudioContext 必须在用户手势后创建/resume，
// 由 unlockRingtoneAudio() 在页面首次交互（点击/按键）时完成。

const RING_FREQUENCIES = [440, 480];
const RING_ON_MS = 2000;
const RING_OFF_MS = 4000;
const RING_VOLUME = 0.12;

let sharedContext: AudioContext | null = null;
let activeOscillators: OscillatorNode[] = [];
let activeGain: GainNode | null = null;
let cycleTimer: number | undefined;
let ringing = false;

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

// 在用户手势中调用：创建并恢复 AudioContext，之后来电时铃声才能发声。
export function unlockRingtoneAudio(): void {
  const ctx = ensureContext();
  if (!ctx) return;
  if (ctx.state === "suspended") void ctx.resume().catch(() => {});
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

function startBurst(ctx: AudioContext): void {
  stopBurst();
  const gain = ctx.createGain();
  gain.connect(ctx.destination);
  const start = ctx.currentTime;
  const onSeconds = RING_ON_MS / 1000;
  // 淡入淡出，避免爆音。
  gain.gain.setValueAtTime(0.0001, start);
  gain.gain.exponentialRampToValueAtTime(RING_VOLUME, start + 0.05);
  gain.gain.setValueAtTime(RING_VOLUME, start + onSeconds - 0.1);
  gain.gain.exponentialRampToValueAtTime(0.0001, start + onSeconds);
  for (const frequency of RING_FREQUENCIES) {
    const osc = ctx.createOscillator();
    osc.type = "sine";
    osc.frequency.value = frequency;
    osc.connect(gain);
    osc.start(start);
    osc.stop(start + onSeconds);
    activeOscillators.push(osc);
  }
  activeGain = gain;
}

// 开始循环振铃；重复调用幂等。
export function startRingtone(): void {
  if (ringing) return;
  ringing = true;
  const ctx = ensureContext();
  if (!ctx) return;
  if (ctx.state === "suspended") void ctx.resume().catch(() => {});
  startBurst(ctx);
  cycleTimer = window.setInterval(() => {
    if (!ringing || !sharedContext) return;
    startBurst(sharedContext);
  }, RING_ON_MS + RING_OFF_MS);
}

// 停止振铃并释放振荡器；AudioContext 保留复用。
export function stopRingtone(): void {
  ringing = false;
  if (cycleTimer !== undefined) {
    window.clearInterval(cycleTimer);
    cycleTimer = undefined;
  }
  stopBurst();
}
