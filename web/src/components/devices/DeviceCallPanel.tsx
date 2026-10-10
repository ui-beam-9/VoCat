import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button, Input, StatusDot } from "../ui";
import { cx } from "../../lib/utils";
import { tf, useI18n } from "../../lib/i18n";
import { apiMessage } from "../../api";
import {
  answerCall,
  dialCall,
  hangupCall,
  listCalls,
  validateCallNumber,
  type CallState,
  type VoWiFiCall,
} from "./callApi";
import { callAudioDiagnostics, CallAudioSession, type CallAudioState } from "../../lib/callAudio";
import { DialPad } from "./DialPad";
import type { NewCallHistoryEntry } from "../../lib/callHistory";

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

export interface DeviceCallPanelProps {
  device: { id: string };
  // 可选受控号码：供通讯录/通话记录回填。未提供时组件内部自管。
  number?: string;
  onNumberChange?: (value: string) => void;
  // 通话结束时回调，用于写入本地通话记录。
  onCallRecord?: (entry: NewCallHistoryEntry) => void;
}

export function DeviceCallPanel({
  device,
  number: numberProp,
  onNumberChange,
  onCallRecord,
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

  const sessionRef = useRef<CallAudioSession | null>(null);
  const sessionCallIdRef = useRef<string>("");
  const trackedCallsRef = useRef<Map<string, { startedAt: string; answeredAt?: string; ended: boolean }>>(new Map());

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
  const incomingCall = useMemo(
    () => calls.find((call) => call.state === "ringing" && call.direction === "incoming"),
    [calls],
  );

  // 通话接通后建立音频桥，通话结束或切换通话时释放。
  useEffect(() => {
    if (!deviceId) return;
    const currentId = sessionCallIdRef.current;
    if (activeCall && voWiFiReady) {
      if (activeCall.id === currentId) return;
      sessionRef.current?.stop();
      sessionRef.current = null;
      setMediaError("");
      setAudioState("connecting");
      const session = new CallAudioSession(deviceId, activeCall.id, {
        onState: (state, detail) => {
          setAudioState(state);
          if (state === "error") setMediaError(detail || t("音频连接失败"));
        },
      });
      sessionRef.current = session;
      sessionCallIdRef.current = activeCall.id;
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
  }, [activeCall, voWiFiReady, deviceId, t]);

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
  }, []);

  // 通话进入终态时写入本地通话记录；同一通话只记录一次。
  useEffect(() => {
    if (!onCallRecord) return;
    const tracked = trackedCallsRef.current;
    for (const call of calls) {
      let entry = tracked.get(call.id);
      if (!entry) {
        entry = { startedAt: call.startedAt || new Date().toISOString(), ended: false };
        tracked.set(call.id, entry);
      }
      if (call.state === "active" && !entry.answeredAt) {
        entry.answeredAt = new Date().toISOString();
      }
      const terminal = call.state === "ended" || call.state === "failed";
      if (!terminal || entry.ended) continue;
      entry.ended = true;
      const endedAt = call.endedAt || new Date().toISOString();
      const durationSeconds = entry.answeredAt
        ? Math.max(0, Math.round((new Date(endedAt).getTime() - new Date(entry.answeredAt).getTime()) / 1000))
        : 0;
      const outcome = call.state === "failed" ? "failed" : entry.answeredAt ? "answered" : "missed";
      onCallRecord({
        callId: call.id,
        number: call.number,
        direction: call.direction,
        outcome,
        startedAt: entry.startedAt,
        endedAt,
        durationSeconds,
      });
    }
  }, [calls, onCallRecord]);

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

  const liveCalls = calls.filter((call) => isLiveCall(call.state));
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
        <div className="mb-3 space-y-1 rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 text-xs text-gray-600 dark:border-white/10 dark:bg-white/5 dark:text-gray-300">
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
          if (event.key === "Enter" && !busy && voWiFiReady) void handleDial();
        }}
        placeholder={t("输入号码，如 10086 或 +8613800000000")}
        disabled={!voWiFiReady || busy}
      />
      <DialPad
        className="mt-3"
        disabled={!voWiFiReady || busy}
        callDisabled={!voWiFiReady || busy || !loaded}
        callLabel={t("拨号")}
        onDigit={(digit) => setNumber(number + digit)}
        onPlus={() => setNumber(number + "+")}
        onBackspace={() => setNumber(number.slice(0, -1))}
        onCall={() => void handleDial()}
      />

      {liveCalls.length > 0 ? (
        <div className="mt-3 space-y-2">
          {liveCalls.map((call) => {
            const isIncomingRinging = call.state === "ringing" && call.direction === "incoming";
            return (
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
                  </div>
                </div>
                <div className="flex shrink-0 gap-2">
                  {isIncomingRinging ? (
                    <Button variant="success" size="small" disabled={busy || !voWiFiReady} onClick={() => void handleAnswer(call)}>
                      {t("接听")}
                    </Button>
                  ) : null}
                  <Button variant="danger" size="small" disabled={busy} onClick={() => void handleHangup(call)}>
                    {t("挂断")}
                  </Button>
                </div>
              </div>
            );
          })}
        </div>
      ) : loaded ? (
        <div className="mt-3 text-xs text-gray-400 dark:text-gray-500">{t("当前没有通话")}</div>
      ) : null}
    </div>
  );
}
