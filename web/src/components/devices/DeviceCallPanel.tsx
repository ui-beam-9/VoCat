import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button, Input, StatusDot } from "../ui";
import { cx } from "../../lib/utils";
import { useI18n } from "../../lib/i18n";
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
import { CallAudioSession, type CallAudioState } from "../../lib/callAudio";
import type { DeviceDetail } from "./types";

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

export function DeviceCallPanel({ device }: { device: DeviceDetail }) {
  const { t } = useI18n();
  const [calls, setCalls] = useState<VoWiFiCall[]>([]);
  const [transport, setTransport] = useState<string>("");
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [mediaError, setMediaError] = useState("");
  const [audioState, setAudioState] = useState<CallAudioState | "idle">("idle");
  const [dialNumber, setDialNumber] = useState("");
  const [busy, setBusy] = useState(false);

  const sessionRef = useRef<CallAudioSession | null>(null);
  const sessionCallIdRef = useRef<string>("");

  const deviceId = device.id;
  const voWiFiReady = transport === "vowifi";

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

  const handleDial = useCallback(async () => {
    const number = dialNumber.trim();
    if (!validateCallNumber(number).valid) {
      setError(t("号码格式不正确：请输入 2-32 位数字，可含首位 +、*、#"));
      return;
    }
    setBusy(true);
    setError("");
    try {
      await dialCall(deviceId, number);
      await refresh();
    } catch (err) {
      setError(apiMessage(err));
    } finally {
      setBusy(false);
    }
  }, [deviceId, dialNumber, refresh, t]);

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

  return (
    <div className="ui-panel-muted p-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="text-xs font-bold uppercase tracking-wider text-gray-500">{t("语音通话")}</div>
        <div className="flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
          <StatusDot tone={audioTone} size="sm" animated={audioState === "connecting"} />
          {audioLabel}
        </div>
      </div>

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

      <div className="flex items-stretch gap-2">
        <Input
          value={dialNumber}
          onChange={(event) => setDialNumber(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !busy && voWiFiReady) void handleDial();
          }}
          placeholder={t("输入号码，如 10086 或 +8613800000000")}
          disabled={!voWiFiReady || busy}
        />
        <Button variant="primary" onClick={() => void handleDial()} loading={busy} disabled={!voWiFiReady || !loaded}>
          {t("拨号")}
        </Button>
      </div>

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
