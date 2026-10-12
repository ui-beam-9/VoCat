import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button, Input, Select, StatusDot } from "../ui";
import { cx } from "../../lib/utils";
import { tf, useI18n } from "../../lib/i18n";
import { apiMessage } from "../../api";
import {
  answerCall,
  dialCall,
  hangupCall,
  holdCall,
  listCalls,
  resumeCall,
  sendDtmf,
  validateCallNumber,
  type CallState,
  type VoWiFiCall,
} from "./callApi";
import {
  audioOutputSelectionSupported,
  callAudioDiagnostics,
  CallAudioSession,
  listAudioDevices,
  measureMicLevel,
  mediaErrorMessage,
  playTestTone,
  type AudioDeviceInfo,
  type CallAudioState,
} from "../../lib/callAudio";
import { fetchPhoneInfo } from "../../lib/phoneInfo";
import { DialPad } from "./DialPad";

const POLL_INTERVAL_MS = 2000;

function callStateLabel(state: CallState, direction: string, t: (text: string) => string): string {
  switch (state) {
    case "dialing":
      return t("正在拨号");
    case "ringing":
      return direction === "incoming" ? t("来电振铃") : t("对方振铃");
    case "early_media":
      return t("接通中");
    case "active":
      return t("通话中");
    case "ended":
      return t("已结束");
    case "failed":
      return t("呼叫失败");
    default:
      return state;
  }
}

function isLiveCall(state: CallState): boolean {
  return state !== "ended" && state !== "failed";
}

function formatCallDuration(totalSeconds: number): string {
  const seconds = Math.max(0, Math.floor(totalSeconds));
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  const pad = (value: number) => String(value).padStart(2, "0");
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(rest)}` : `${pad(minutes)}:${pad(rest)}`;
}

export interface DeviceCallPanelProps {
  device: { id: string };
  // 可选受控号码：供通讯录/通话记录回填。未提供时组件内部自管。
  number?: string;
  onNumberChange?: (value: string) => void;
}

export function DeviceCallPanel({
  device,
  number: numberProp,
  onNumberChange,
}: DeviceCallPanelProps) {
  const { t } = useI18n();
  const [calls, setCalls] = useState<VoWiFiCall[]>([]);
  const [transport, setTransport] = useState<string>("");
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [mediaError, setMediaError] = useState("");
  const [audioState, setAudioState] = useState<CallAudioState | "idle">("idle");
  const [internalNumber, setInternalNumber] = useState("");
  const [busy, setBusy] = useState(false);
  const [holdBusy, setHoldBusy] = useState(false);
  // 麦克风电平与增益（仅在通话中展示）。
  const [micLevel, setMicLevel] = useState(0);
  const [micGain, setMicGain] = useState(1);
  // 通话时长计时器与号码归属地。
  const [nowTick, setNowTick] = useState(() => Date.now());
  const [phoneInfoText, setPhoneInfoText] = useState("");
  const [phoneInfoLoading, setPhoneInfoLoading] = useState(false);
  // 音频诊断：设备选择与测试状态
  const [inputDevices, setInputDevices] = useState<AudioDeviceInfo[]>([]);
  const [outputDevices, setOutputDevices] = useState<AudioDeviceInfo[]>([]);
  const [selectedInput, setSelectedInput] = useState("");
  const [selectedOutput, setSelectedOutput] = useState("");
  const [diagTesting, setDiagTesting] = useState(false);
  const [diagPhase, setDiagPhase] = useState<"idle" | "tone" | "mic" | "done" | "fail">("idle");
  const [diagLevel, setDiagLevel] = useState(0);
  const [diagResult, setDiagResult] = useState("");

  const sessionRef = useRef<CallAudioSession | null>(null);
  const sessionCallIdRef = useRef<string>("");
  const routeRef = useRef<{ inputDeviceId?: string; outputDeviceId?: string }>({});
  const micGainRef = useRef(1);

  const deviceId = device.id;
  const number = numberProp ?? internalNumber;
  const setNumber = useCallback(
    (value: string) => {
      if (numberProp === undefined) setInternalNumber(value);
      onNumberChange?.(value);
    },
    [numberProp, onNumberChange],
  );
  const voWiFiReady = transport === "vowifi";
  const audioDiag = useMemo(() => callAudioDiagnostics(), []);
  const outputSelectionSupported = useMemo(() => audioOutputSelectionSupported(), []);
  const [diagOpen, setDiagOpen] = useState(false);

  const refresh = useCallback(async () => {
    if (!deviceId) return;
    try {
      const result = await listCalls(deviceId);
      setCalls(result.calls ?? []);
      setTransport(result.transport ?? "");
      setError("");
    } catch (err) {
      setError(apiMessage(err));
    } finally {
      setLoaded(true);
    }
  }, [deviceId]);

  const refreshDevices = useCallback(async () => {
    try {
      const lists = await listAudioDevices();
      setInputDevices(lists.inputs);
      setOutputDevices(lists.outputs);
    } catch {
      /* 忽略：部分浏览器枚举设备失败 */
    }
  }, []);

  const runAudioTest = useCallback(async () => {
    if (!audioDiag.supported) return;
    setDiagTesting(true);
    setDiagLevel(0);
    setDiagResult("");
    try {
      // 第一步：播放测试音，走所选播放设备。
      setDiagPhase("tone");
      await playTestTone(selectedOutput || undefined);
      // 第二步：麦克风测试，需要用户说话并被检测到才算成功。
      setDiagPhase("mic");
      const result = await measureMicLevel({
        deviceId: selectedInput || undefined,
        durationMs: 4000,
        onLevel: (level) => setDiagLevel(level),
      });
      await refreshDevices();
      if (result.detected) {
        setDiagPhase("done");
        setDiagResult(t("测试成功：已检测到麦克风声音"));
      } else {
        setDiagPhase("fail");
        setDiagResult(t("测试失败：未检测到麦克风声音，请对着麦克风说话后重试"));
      }
    } catch (err) {
      setDiagPhase("fail");
      setDiagResult(mediaErrorMessage(err));
    } finally {
      setDiagTesting(false);
      setDiagLevel(0);
    }
  }, [audioDiag.supported, selectedInput, selectedOutput, refreshDevices, t]);

  // 所选音频设备在通话建立时读取（存 ref，避免切换设备导致进行中的通话音频重建）。
  useEffect(() => {
    routeRef.current = {
      inputDeviceId: selectedInput || undefined,
      outputDeviceId: selectedOutput || undefined,
    };
  }, [selectedInput, selectedOutput]);

  // 麦克风增益存 ref，便于新建音频会话时套用当前值。
  useEffect(() => {
    micGainRef.current = micGain;
    sessionRef.current?.setMicGain(micGain);
  }, [micGain]);

  // 打开诊断面板时枚举一次设备（未授权时 label 可能为空）。
  useEffect(() => {
    if (diagOpen) void refreshDevices();
  }, [diagOpen, refreshDevices]);

  // 轮询通话状态；页面隐藏时暂停，避免无谓请求。
  useEffect(() => {
    if (!deviceId) return;
    void refresh();
    let timer: number | undefined;
    const start = () => {
      if (timer !== undefined) return;
      timer = window.setInterval(() => void refresh(), POLL_INTERVAL_MS);
    };
    const stop = () => {
      if (timer !== undefined) {
        window.clearInterval(timer);
        timer = undefined;
      }
    };
    const onVisibility = () => (document.hidden ? stop() : start());
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [deviceId, refresh]);

  const activeCall = useMemo(
    () => calls.find((call) => call.state === "active"),
    [calls],
  );
  const liveCalls = useMemo(() => calls.filter((call) => isLiveCall(call.state)), [calls]);
  // 来电振铃：主按钮区显示「接听」+「挂断」。
  const incomingRinging = useMemo(
    () => calls.find((call) => call.state === "ringing" && call.direction === "incoming"),
    [calls],
  );
  // 主按钮可挂断的通话：优先已接通，其次是呼出中的通话。
  const controllableCall = useMemo(
    () =>
      activeCall
      ?? liveCalls.find((call) => call.direction === "outgoing"),
    [activeCall, liveCalls],
  );

  const activeCallId = activeCall?.id ?? "";
  const activeCallStarted = activeCall?.answeredAt || activeCall?.startedAt || "";
  // 音频会话挂接的通话：优先已接通，其次带早期媒体的呼出（回铃音阶段就能听到声音）。
  // early_media → active 是同一通话 id，接通后无需重连。
  const mediaCall = useMemo(
    () => activeCall ?? calls.find((call) => call.state === "early_media"),
    [activeCall, calls],
  );

  // 通话中每秒刷新一次计时，用于显示通话时长。
  useEffect(() => {
    if (!activeCallId) return;
    setNowTick(Date.now());
    const timer = window.setInterval(() => setNowTick(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [activeCallId]);

  const callDurationSeconds = useMemo(() => {
    if (!activeCallStarted) return 0;
    const started = new Date(activeCallStarted).getTime();
    if (Number.isNaN(started)) return 0;
    return Math.max(0, Math.floor((nowTick - started) / 1000));
  }, [activeCallStarted, nowTick]);

  // 通话接通后查询号码归属地（结果在 lib/phoneInfo 内缓存）；失败时通话期间自动重试，避免偶发失败一直显示「归属地未知」。
  useEffect(() => {
    const target = (activeCall?.number ?? "").trim();
    if (!target) {
      setPhoneInfoText("");
      setPhoneInfoLoading(false);
      return;
    }
    let cancelled = false;
    let attempts = 0;
    let retryTimer: number | undefined;
    const load = () => {
      setPhoneInfoLoading(true);
      fetchPhoneInfo(target)
        .then((info) => {
          if (cancelled) return;
          setPhoneInfoText(info.display || "");
          setPhoneInfoLoading(false);
        })
        .catch(() => {
          if (cancelled) return;
          attempts += 1;
          if (attempts < 3) {
            retryTimer = window.setTimeout(load, 3000);
            return;
          }
          setPhoneInfoText("");
          setPhoneInfoLoading(false);
        });
    };
    load();
    return () => {
      cancelled = true;
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
    };
  }, [activeCall?.number]);

  // 通话接通或进入早期媒体（回铃音）后建立音频桥，通话结束或切换通话时释放。
  useEffect(() => {
    if (!deviceId) return;
    const currentId = sessionCallIdRef.current;
    if (mediaCall && voWiFiReady) {
      if (mediaCall.id === currentId) return;
      sessionRef.current?.stop();
      sessionRef.current = null;
      setMediaError("");
      setAudioState("connecting");
      const session = new CallAudioSession(
        deviceId,
        mediaCall.id,
        {
          onState: (state, detail) => {
            setAudioState(state);
            if (state === "error") setMediaError(detail || t("音频连接失败"));
            if (state === "closed") setMicLevel(0);
          },
          onMicLevel: (level) => setMicLevel(level),
        },
        routeRef.current,
      );
      sessionRef.current = session;
      sessionCallIdRef.current = mediaCall.id;
      session.setMicGain(micGainRef.current);
      session.start().catch((err) => {
        setMediaError(err instanceof Error ? err.message : String(err));
        setAudioState("error");
        sessionRef.current = null;
        sessionCallIdRef.current = "";
      });
      return;
    }
    if (currentId) {
      sessionRef.current?.stop();
      sessionRef.current = null;
      sessionCallIdRef.current = "";
      setAudioState("idle");
    }
  }, [mediaCall, voWiFiReady, deviceId, t]);

  useEffect(() => {
    return () => {
      sessionRef.current?.stop();
      sessionRef.current = null;
      sessionCallIdRef.current = "";
    };
  }, []);

  const stopMedia = useCallback(() => {
    sessionRef.current?.stop();
    sessionRef.current = null;
    sessionCallIdRef.current = "";
    setAudioState("idle");
    setMicLevel(0);
  }, []);

  const handleDial = useCallback(async () => {
    const target = number.trim();
    if (!validateCallNumber(target).valid) {
      setError(t("号码格式不正确：请输入 2-32 位数字，可含首位 +、*、#"));
      return;
    }
    setBusy(true);
    setError("");
    try {
      await dialCall(deviceId, target);
      await refresh();
    } catch (err) {
      setError(apiMessage(err));
    } finally {
      setBusy(false);
    }
  }, [deviceId, number, refresh, t]);

  const handleAnswer = useCallback(
    async (call: VoWiFiCall) => {
      setBusy(true);
      setError("");
      try {
        await answerCall(deviceId, call.id);
        await refresh();
      } catch (err) {
        setError(apiMessage(err));
      } finally {
        setBusy(false);
      }
    },
    [deviceId, refresh],
  );

  const handleHangup = useCallback(
    async (call: VoWiFiCall) => {
      setBusy(true);
      setError("");
      stopMedia();
      try {
        await hangupCall(deviceId, call.id);
        await refresh();
      } catch (err) {
        setError(apiMessage(err));
      } finally {
        setBusy(false);
      }
    },
    [deviceId, refresh, stopMedia],
  );

  const handleToggleHold = useCallback(async () => {
    if (!activeCall) return;
    setHoldBusy(true);
    setError("");
    try {
      if (activeCall.held) await resumeCall(deviceId, activeCall.id);
      else await holdCall(deviceId, activeCall.id);
      await refresh();
    } catch (err) {
      setError(apiMessage(err));
    } finally {
      setHoldBusy(false);
    }
  }, [activeCall, deviceId, refresh]);

  // 通话中按拨号盘发送 DTMF；未通话时正常拼接到号码。
  const handleDigit = useCallback(
    (digit: string) => {
      if (activeCall) {
        setError("");
        void sendDtmf(deviceId, activeCall.id, digit).catch((err) => {
          setError(apiMessage(err));
        });
        return;
      }
      setNumber(number + digit);
    },
    [activeCall, deviceId, number, setNumber],
  );

  const handlePrimaryCall = useCallback(() => {
    if (controllableCall) void handleHangup(controllableCall);
    else void handleDial();
  }, [controllableCall, handleDial, handleHangup]);

  const audioTone: "success" | "warning" | "danger" | "neutral" =
    audioState === "live" ? "success" : audioState === "connecting" ? "warning" : audioState === "error" ? "danger" : "neutral";
  const audioLabel =
    audioState === "live"
      ? t("音频已接通")
      : audioState === "connecting"
        ? t("正在建立音频…")
        : audioState === "error"
          ? t("音频连接失败")
          : t("未在通话");
  const audioReasonText =
    audioDiag.issue === "insecure_context"
      ? tf("当前以 {origin} 访问，不是安全上下文；浏览器只在 HTTPS 或 localhost 下提供麦克风/扬声器", {
          origin: audioDiag.origin || "http://...",
        })
      : audioDiag.issue === "no_media_devices"
        ? t("浏览器未提供 navigator.mediaDevices 接口")
        : audioDiag.issue === "no_get_user_media"
          ? t("浏览器未提供 getUserMedia 接口")
          : "";

  return (
    <div className="ui-panel-muted p-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="text-xs font-bold uppercase tracking-wider text-gray-500">{t("语音通话")}</div>
        <div className="flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
          <span className="flex items-center gap-1.5">
            <StatusDot tone={audioTone} size="sm" animated={audioState === "connecting"} />
            {audioLabel}
          </span>
          <button
            type="button"
            onClick={() => setDiagOpen((value) => !value)}
            className="rounded px-1 text-[#0ea5e9] transition-colors hover:bg-black/5 dark:hover:bg-white/10"
          >
            {t("音频诊断")}
          </button>
        </div>
      </div>

      {diagOpen ? (
        <div className="mb-3 space-y-2 rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 text-xs text-gray-600 dark:border-white/10 dark:bg-white/5 dark:text-gray-300">
          <div>
            {t("访问地址")}：<span className="font-mono">{audioDiag.origin || "-"}</span>
          </div>
          <div>
            {t("安全上下文")}：{audioDiag.secureContext ? t("是") : t("否")}
          </div>
          <div>
            {t("麦克风接口")}：{audioDiag.hasGetUserMedia ? t("可用") : t("不可用")}
          </div>
          <div className={audioDiag.supported ? "text-emerald-600 dark:text-emerald-400" : "text-amber-600 dark:text-amber-400"}>
            {audioDiag.supported ? t("网页音频可用") : audioReasonText}
          </div>
          {audioDiag.supported ? (
            <div className="space-y-2 pt-1">
              <div>
                <div className="mb-1 text-gray-500 dark:text-gray-400">{t("播放设备")}</div>
                <Select
                  value={selectedOutput}
                  onChange={setSelectedOutput}
                  disabled={!outputSelectionSupported || diagTesting}
                  options={[
                    { value: "", label: t("系统默认") },
                    ...outputDevices.map((device, index) => ({
                      value: device.deviceId,
                      label: device.label || `#${index + 1}`,
                    })),
                  ]}
                />
                {!outputSelectionSupported ? (
                  <div className="mt-1 text-amber-600 dark:text-amber-400">
                    {t("当前浏览器不支持选择播放设备，将使用系统默认输出")}
                  </div>
                ) : null}
              </div>
              <div>
                <div className="mb-1 text-gray-500 dark:text-gray-400">{t("麦克风设备")}</div>
                <Select
                  value={selectedInput}
                  onChange={setSelectedInput}
                  disabled={diagTesting}
                  options={[
                    { value: "", label: t("系统默认") },
                    ...inputDevices.map((device, index) => ({
                      value: device.deviceId,
                      label: device.label || `#${index + 1}`,
                    })),
                  ]}
                />
              </div>
              <div className="flex items-center gap-2">
                <Button size="small" disabled={diagTesting} onClick={() => void runAudioTest()}>
                  {diagTesting ? t("测试中…") : t("测试音频")}
                </Button>
                {diagPhase === "tone" ? <span>{t("正在播放测试音…")}</span> : null}
                {diagPhase === "mic" ? <span>{t("请对着麦克风说话…")}</span> : null}
              </div>
              {diagTesting ? (
                <div className="h-2 w-full overflow-hidden rounded bg-gray-200 dark:bg-white/10">
                  <div
                    className="h-full bg-emerald-500 transition-[width] duration-75"
                    style={{ width: `${Math.min(100, Math.round(diagLevel * 500))}%` }}
                  />
                </div>
              ) : null}
              {diagPhase === "done" && diagResult ? (
                <div className="text-emerald-600 dark:text-emerald-400">{diagResult}</div>
              ) : null}
              {diagPhase === "fail" && diagResult ? (
                <div className="text-red-600 dark:text-red-400">{diagResult}</div>
              ) : null}
            </div>
          ) : null}
        </div>
      ) : null}

      {!audioDiag.supported ? (
        <div className="mb-3 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-800 dark:border-amber-500/25 dark:bg-amber-500/10 dark:text-amber-200">
          {tf("网页音频不可用：{reason}。拨号信令仍可用，但无法进行语音通话。", { reason: audioReasonText })}
        </div>
      ) : null}

      {!voWiFiReady ? (
        <div className="mb-3 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-800 dark:border-amber-500/25 dark:bg-amber-500/10 dark:text-amber-200">
          {t("网页拨号需要 VoWiFi（WiFi Calling）已注册。当前通话走蜂窝网络时只能发起信令、无法在浏览器收发语音。")}
        </div>
      ) : null}

      {error ? (
        <div className="mb-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs leading-5 text-red-700 dark:border-red-500/25 dark:bg-red-500/10 dark:text-red-300">
          {error}
        </div>
      ) : null}
      {mediaError ? (
        <div className="mb-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs leading-5 text-red-700 dark:border-red-500/25 dark:bg-red-500/10 dark:text-red-300">
          {mediaError}
        </div>
      ) : null}

      <Input
        value={number}
        onChange={(event) => setNumber(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !busy && voWiFiReady && !controllableCall) void handleDial();
        }}
        placeholder={t("输入号码，如 10086 或 +8613800000000")}
        disabled={!voWiFiReady || busy || !!activeCall}
      />
      <DialPad
        className="mt-3"
        mode={activeCall ? "dtmf" : "dial"}
        disabled={!voWiFiReady || busy}
        callDisabled={!voWiFiReady || busy || (!incomingRinging && !controllableCall && !loaded)}
        callVariant={incomingRinging || controllableCall ? "danger" : "primary"}
        callLabel={incomingRinging || controllableCall ? t("挂断") : t("拨号")}
        onAnswer={incomingRinging ? () => void handleAnswer(incomingRinging) : undefined}
        answerLabel={t("接听")}
        answerDisabled={busy || !voWiFiReady}
        onDigit={handleDigit}
        onPlus={() => setNumber(number + "+")}
        onBackspace={() => setNumber(number.slice(0, -1))}
        onCall={incomingRinging ? () => void handleHangup(incomingRinging) : handlePrimaryCall}
      />

      {activeCall ? (
        <div className="mt-3 space-y-2 rounded-lg border border-gray-200 bg-white px-3 py-2 text-xs text-gray-600 dark:border-white/10 dark:bg-white/5 dark:text-gray-300">
          <div className="flex items-center gap-2">
            <span className="w-14 shrink-0 text-gray-500 dark:text-gray-400">{t("麦克风")}</span>
            <div className="h-2 flex-1 overflow-hidden rounded bg-gray-200 dark:bg-white/10">
              <div
                className="h-full bg-emerald-500 transition-[width] duration-75"
                style={{ width: `${Math.min(100, Math.round(micLevel * 500))}%` }}
              />
            </div>
          </div>
          <div className="flex items-center gap-2">
            <span className="w-14 shrink-0 text-gray-500 dark:text-gray-400">{t("音量")}</span>
            <input
              type="range"
              min={0}
              max={2}
              step={0.1}
              value={micGain}
              onChange={(event) => setMicGain(Number(event.target.value))}
              className="h-1.5 flex-1 cursor-pointer accent-[#0ea5e9]"
              aria-label={t("麦克风音量")}
            />
            <span className="w-10 shrink-0 text-right font-mono">{Math.round(micGain * 100)}%</span>
          </div>
          <div className="flex items-center justify-between">
            <span className="text-gray-500 dark:text-gray-400">
              {t("通话时长")} <span className="font-mono text-gray-700 dark:text-gray-200">{formatCallDuration(callDurationSeconds)}</span>
            </span>
            <Button size="small" loading={holdBusy} onClick={() => void handleToggleHold()}>
              {activeCall.held ? t("恢复通话") : t("暂停通话")}
            </Button>
          </div>
        </div>
      ) : null}

      {liveCalls.length > 0 ? (
        <div className="mt-3 space-y-2">
          {liveCalls.map((call) => (
            <div
              key={call.id}
              className={cx(
                "flex items-center justify-between gap-3 rounded-lg border px-3 py-2",
                call.state === "active"
                  ? "border-emerald-200 bg-emerald-50 dark:border-emerald-500/25 dark:bg-emerald-500/10"
                  : "border-gray-200 bg-white dark:border-white/10 dark:bg-white/5",
              )}
            >
              <div className="min-w-0">
                <div className="truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                  {call.number || t("未知号码")}
                </div>
                <div className="text-xs text-gray-500 dark:text-gray-400">
                  {call.direction === "incoming" ? t("来电") : t("呼出")} · {callStateLabel(call.state, call.direction, t)}
                  {call.state === "active" ? ` · ${formatCallDuration(
                    Math.max(0, Math.floor((nowTick - new Date(call.answeredAt || call.startedAt || Date.now()).getTime()) / 1000)),
                  )}` : ""}
                  {call.held ? ` · ${t("已暂停")}` : ""}
                </div>
              </div>
              {/* 归属地：接听/挂断按钮统一放在拨号盘下方，这里只展示归属地。 */}
              {call.id === activeCall?.id ? (
                <span
                  className="max-w-[12rem] shrink-0 truncate text-xs text-gray-500 dark:text-gray-400"
                  title={phoneInfoText}
                >
                  {phoneInfoLoading ? t("归属地查询中…") : phoneInfoText || t("归属地未知")}
                </span>
              ) : null}
            </div>
          ))}
        </div>
      ) : loaded ? (
        <div className="mt-3 text-xs text-gray-400 dark:text-gray-500">{t("当前没有通话")}</div>
      ) : null}
    </div>
  );
}
