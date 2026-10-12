import { useEffect, useRef, useState } from "react";
import { api } from "../api";
import { tl, useI18n } from "../lib/i18n";
import { startRingtone, stopRingtone, unlockRingtoneAudio } from "../lib/ringtone";
import { listCalls } from "./devices/callApi";
import type { DeviceListItem, DevicesResponse } from "../types";

const DEVICE_POLL_MS = 5000;
const CALL_POLL_MS = 2000;
const PERMISSION_ASKED_KEY = "vocat_incoming_notify_asked";

// 非组件回调里取翻译：词典缺失时回退中文原文。
function notifyIncoming(number: string): void {
  if (typeof Notification === "undefined" || Notification.permission !== "granted") return;
  try {
    const notification = new Notification(tl("来电"), {
      body: number || tl("未知号码"),
      // tag 按来电区分：同通话不重复弹，多通来电可并存。
      tag: `vocat-incoming-${number || "unknown"}`,
    });
    notification.onclick = () => window.focus();
  } catch {
    /* 部分平台要求经 ServiceWorker 发通知，失败时静默降级为仅铃声 */
  }
}

// 全局来电监听：登录后挂在 AuthenticatedShell 下，任意页面（含后台标签页）生效。
// 检测到新来电振铃时发系统通知 + 播放振铃音；所有来电离开振铃状态后停止铃声。
export function IncomingCallWatcher() {
  const { t } = useI18n();
  const [showBanner, setShowBanner] = useState(false);
  const devicesRef = useRef<DeviceListItem[]>([]);
  // 已触发提醒的来电（deviceId:callId）；离开 ringing 状态后移除，下次来电可再提醒。
  const notifiedRef = useRef<Set<string>>(new Set());
  const ringingCountRef = useRef(0);

  // 通知权限横幅：仅未决定且未请求过时显示一次。
  useEffect(() => {
    if (typeof Notification === "undefined") return;
    let asked = false;
    try {
      asked = localStorage.getItem(PERMISSION_ASKED_KEY) === "1";
    } catch {
      asked = false;
    }
    if (Notification.permission === "default" && !asked) setShowBanner(true);
  }, []);

  // 页面首次交互解锁音频（autoplay 限制），之后来电铃声才能发声。
  useEffect(() => {
    const unlock = () => unlockRingtoneAudio();
    window.addEventListener("pointerdown", unlock, { once: true });
    window.addEventListener("keydown", unlock, { once: true });
    return () => {
      window.removeEventListener("pointerdown", unlock);
      window.removeEventListener("keydown", unlock);
    };
  }, []);

  // 设备列表轮询（wifi_410 不支持通话，排除）。
  useEffect(() => {
    let active = true;
    const load = () =>
      api<DevicesResponse>("/devices")
        .then((res) => {
          if (!active) return;
          devicesRef.current = (res.devices ?? []).filter((device) => device.deviceType !== "wifi_410");
        })
        .catch(() => {});
    void load();
    const timer = window.setInterval(() => void load(), DEVICE_POLL_MS);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, []);

  // 通话轮询：检测新出现的 incoming ringing；后台标签页继续轮询（来电提醒核心场景）。
  useEffect(() => {
    let active = true;
    const check = async () => {
      const devices = devicesRef.current;
      if (devices.length === 0) return;
      const results = await Promise.allSettled(devices.map((device) => listCalls(device.id)));
      if (!active) return;
      let ringingCount = 0;
      const currentKeys = new Set<string>();
      results.forEach((result, index) => {
        if (result.status !== "fulfilled") return;
        const deviceId = devices[index].id;
        for (const call of result.value.calls ?? []) {
          if (call.state !== "ringing" || call.direction !== "incoming") continue;
          ringingCount += 1;
          const key = `${deviceId}:${call.id}`;
          currentKeys.add(key);
          if (!notifiedRef.current.has(key)) {
            notifiedRef.current.add(key);
            notifyIncoming(call.number);
          }
        }
      });
      for (const key of notifiedRef.current) {
        if (!currentKeys.has(key)) notifiedRef.current.delete(key);
      }
      // 有来电振铃则响铃，全部消失（接听/挂断/结束）则停止。
      if (ringingCount > 0 && ringingCountRef.current === 0) startRingtone();
      if (ringingCount === 0 && ringingCountRef.current > 0) stopRingtone();
      ringingCountRef.current = ringingCount;
    };
    void check();
    const timer = window.setInterval(() => void check(), CALL_POLL_MS);
    return () => {
      active = false;
      window.clearInterval(timer);
      stopRingtone();
    };
  }, []);

  const enableNotifications = async () => {
    try {
      localStorage.setItem(PERMISSION_ASKED_KEY, "1");
    } catch {
      /* ignore */
    }
    // 按钮点击本身是用户手势，顺便解锁音频。
    unlockRingtoneAudio();
    if (typeof Notification !== "undefined" && Notification.permission === "default") {
      try {
        await Notification.requestPermission();
      } catch {
        /* 浏览器拒绝或异常时静默 */
      }
    }
    setShowBanner(false);
  };

  const dismissBanner = () => {
    try {
      localStorage.setItem(PERMISSION_ASKED_KEY, "1");
    } catch {
      /* ignore */
    }
    setShowBanner(false);
  };

  if (!showBanner) return null;
  return (
    <div className="fixed bottom-5 right-5 z-40 w-72 rounded-xl border border-gray-200 bg-white p-4 shadow-lg dark:border-white/10 dark:bg-[#1a1a20]">
      <div className="text-sm font-bold text-gray-900 dark:text-gray-100">{t("启用桌面通知")}</div>
      <div className="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">
        {t("来电时显示系统通知并播放振铃音")}
      </div>
      <div className="mt-3 flex items-center justify-end gap-2">
        <button
          type="button"
          onClick={dismissBanner}
          className="rounded-lg px-3 py-1.5 text-xs text-gray-500 transition-colors hover:bg-black/5 dark:text-gray-400 dark:hover:bg-white/10"
        >
          {t("以后再说")}
        </button>
        <button
          type="button"
          onClick={() => void enableNotifications()}
          className="rounded-lg bg-[#0ea5e9] px-3 py-1.5 text-xs font-medium text-white transition-colors hover:bg-[#0284c7]"
        >
          {t("启用")}
        </button>
      </div>
    </div>
  );
}
