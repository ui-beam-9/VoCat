// VoCat 通话音频 AudioWorklet 处理器。
//
// 通过同源静态文件加载（addModule('/call-worklet.js')），以满足服务端 CSP
// 的 script-src 'self'；若改用 Blob URL 会被 CSP 拦截导致加载失败。
//
// 契约（见 internal/server/call_media_api.go 与 src/lib/callAudio.ts）：
// - 采集：把麦克风输入重采样到 8kHz，按 20ms（160 样本）打包成 Int16 PCM 发回主线程。
// - 播放：接收 8kHz Int16 PCM，重采样到宿主采样率后输出。

const TARGET_SAMPLE_RATE = 8000;
const FRAME_SAMPLES = 160; // 20ms @ 8kHz

class CaptureProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ratio = sampleRate / TARGET_SAMPLE_RATE;
    this.inBuf = new Float32Array(0);
    this.pos = 0;
    this.frame = new Float32Array(FRAME_SAMPLES);
    this.frameLen = 0;
  }

  drain() {
    while (this.pos + 1 < this.inBuf.length) {
      const i = Math.floor(this.pos);
      const frac = this.pos - i;
      this.frame[this.frameLen++] = this.inBuf[i] * (1 - frac) + this.inBuf[i + 1] * frac;
      this.pos += this.ratio;
      if (this.frameLen === FRAME_SAMPLES) {
        const pcm = new Int16Array(FRAME_SAMPLES);
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
registerProcessor("vocat-capture", CaptureProcessor);

class PlaybackProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.step = TARGET_SAMPLE_RATE / sampleRate;
    this.buf = new Float32Array(0);
    this.pos = 0;
    this.maxBuf = TARGET_SAMPLE_RATE * 3;
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
registerProcessor("vocat-playback", PlaybackProcessor);
