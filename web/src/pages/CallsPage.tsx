import { useCallback, useEffect, useMemo, useState } from "react";
import { api, apiMessage } from "../api";
import { useI18n } from "../lib/i18n";
import { PageHeader, StatusDot } from "../components/ui";
import { DeviceCallPanel } from "../components/devices/DeviceCallPanel";
import { cx } from "../lib/utils";
import type { DeviceListItem, DevicesResponse } from "../types";

const POLL_INTERVAL_MS = 5000;

// 全局"语音通话"页：选择设备后在浏览器内拨号/接听/通话。
// 通话音频依赖 VoWiFi（IMS 已注册），仅 VoWiFi 就绪时可用。
export default function CallsPage() {
  const { t } = useI18n();
  const [devices, setDevices] = useState<DeviceListItem[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const res = await api<DevicesResponse>("/devices");
      // wifi_410 后端不支持通话，直接排除。
      const list = (res.devices ?? []).filter((device) => device.deviceType !== "wifi_410");
      setDevices(list);
      setError("");
      setSelectedId((current) =>
        current && list.some((device) => device.id === current) ? current : list[0]?.id ?? "",
      );
    } catch (err) {
      setError(apiMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => {
      if (!document.hidden) void refresh();
    }, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [refresh]);

  const selected = useMemo(
    () => devices.find((device) => device.id === selectedId) ?? null,
    [devices, selectedId],
  );

  return (
    <div className="mx-auto w-full max-w-[1500px]">
      <PageHeader
        title={t("语音通话")}
        subtitle={t("通过 VoWiFi（WiFi Calling）在浏览器内拨号、接听与通话")}
      />
      {error ? (
        <div className="mb-4 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-500/25 dark:bg-red-500/10 dark:text-red-300">
          {error}
        </div>
      ) : null}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[280px_1fr]">
        <div className="ui-card p-3">
          <div className="mb-2 px-1 text-xs font-bold uppercase tracking-wider text-gray-500">
            {t("选择设备")}
          </div>
          {devices.length === 0 ? (
            <div className="px-1 py-4 text-sm text-gray-400">
              {loading ? t("加载中...") : t("暂无可用设备")}
            </div>
          ) : (
            <div className="space-y-1">
              {devices.map((device) => {
                const active = device.id === selectedId;
                const ready = !!device.vowifiEnabled && device.vowifiRuntime?.imsReady === true;
                return (
                  <button
                    key={device.id}
                    type="button"
                    onClick={() => setSelectedId(device.id)}
                    className={cx(
                      "flex w-full items-center gap-2 rounded-lg border px-3 py-2 text-left transition-colors",
                      active
                        ? "border-[#0ea5e9] bg-[#f0f9ff] dark:border-[#0ea5e9]/50 dark:bg-[#0ea5e9]/10"
                        : "border-transparent hover:bg-black/5 dark:hover:bg-white/5",
                    )}
                  >
                    <StatusDot tone={ready ? "success" : "neutral"} size="sm" animated={false} />
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                        {device.name || device.id}
                      </span>
                      <span className="block truncate font-mono text-[10px] text-gray-400">{device.id}</span>
                    </span>
                  </button>
                );
              })}
            </div>
          )}
        </div>
        <div>
          {selected ? (
            <DeviceCallPanel key={selected.id} device={selected} />
          ) : (
            <div className="ui-card p-10 text-center text-sm text-gray-400">{t("请选择左侧设备查看详情")}</div>
          )}
        </div>
      </div>
    </div>
  );
}
