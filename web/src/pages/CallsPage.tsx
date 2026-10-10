import { useCallback, useEffect, useMemo, useState } from "react";
import { api, apiMessage } from "../api";
import { useI18n } from "../lib/i18n";
import { Button, PageHeader, StatusDot, Tabs } from "../components/ui";
import { DeviceCallPanel } from "../components/devices/DeviceCallPanel";
import { listContacts } from "../components/sms/smsApi";
import { cx } from "../lib/utils";
import {
  appendCallHistory,
  clearCallHistory,
  loadCallHistory,
  newCallHistoryId,
  type CallHistoryEntry,
  type CallOutcome,
  type NewCallHistoryEntry,
} from "../lib/callHistory";
import type { DeviceListItem, DevicesResponse, SMSContact } from "../types";

const POLL_INTERVAL_MS = 5000;

function formatDuration(seconds: number): string {
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return `${minutes}:${String(rest).padStart(2, "0")}`;
}

function formatTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

// 全局"语音通话"页：设备选择 + 拨号盘 + 通话记录 + 通讯录。
// 音频依赖 VoWiFi（IMS 已注册）；通话记录仅存本地浏览器。
export default function CallsPage() {
  const { t } = useI18n();
  const [devices, setDevices] = useState<DeviceListItem[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [number, setNumber] = useState("");
  const [tab, setTab] = useState<"history" | "contacts">("history");
  const [history, setHistory] = useState<CallHistoryEntry[]>(() => loadCallHistory());
  const [contacts, setContacts] = useState<SMSContact[]>([]);
  const [contactsLoading, setContactsLoading] = useState(false);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const refreshDevices = useCallback(async () => {
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
    void refreshDevices();
    const timer = window.setInterval(() => {
      if (!document.hidden) void refreshDevices();
    }, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [refreshDevices]);

  const selected = useMemo(
    () => devices.find((device) => device.id === selectedId) ?? null,
    [devices, selectedId],
  );

  const loadContacts = useCallback(async () => {
    if (!selectedId) return;
    setContactsLoading(true);
    try {
      setContacts(await listContacts(selectedId));
    } catch {
      setContacts([]);
    } finally {
      setContactsLoading(false);
    }
  }, [selectedId]);

  useEffect(() => {
    if (tab !== "contacts") return;
    void loadContacts();
  }, [tab, loadContacts]);

  const handleCallRecord = useCallback(
    (entry: NewCallHistoryEntry) => {
      setHistory(appendCallHistory({ ...entry, id: newCallHistoryId(), deviceId: selectedId }));
    },
    [selectedId],
  );

  const outcomeLabel = useCallback(
    (outcome: CallOutcome) =>
      outcome === "answered" ? t("已接通") : outcome === "missed" ? t("未接") : t("呼叫失败"),
    [t],
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
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[260px_1fr]">
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

        <div className="space-y-4">
          {selected ? (
            <DeviceCallPanel
              key={selected.id}
              device={selected}
              number={number}
              onNumberChange={setNumber}
              onCallRecord={handleCallRecord}
            />
          ) : (
            <div className="ui-card p-10 text-center text-sm text-gray-400">{t("请选择左侧设备查看详情")}</div>
          )}

          <div className="ui-card p-4">
            <div className="flex items-center justify-between gap-2">
              <Tabs
                tabs={[
                  { key: "history", label: t("通话记录") },
                  { key: "contacts", label: t("通讯录") },
                ]}
                value={tab}
                onChange={(key) => setTab(key as "history" | "contacts")}
              />
              <div className="shrink-0">
                {tab === "history" ? (
                  <Button
                    size="small"
                    disabled={history.length === 0}
                    onClick={() => setHistory(clearCallHistory())}
                  >
                    {t("清空")}
                  </Button>
                ) : (
                  <Button size="small" loading={contactsLoading} onClick={() => void loadContacts()}>
                    {t("刷新")}
                  </Button>
                )}
              </div>
            </div>
            <div className="mt-4">
              {tab === "history" ? (
                history.length === 0 ? (
                  <div className="py-6 text-center text-sm text-gray-400">{t("暂无通话记录")}</div>
                ) : (
                  <div className="space-y-1">
                    {history.map((entry) => (
                      <button
                        key={entry.id}
                        type="button"
                        onClick={() => setNumber(entry.number)}
                        className="flex w-full items-center justify-between gap-3 rounded-lg px-3 py-2 text-left transition-colors hover:bg-black/5 dark:hover:bg-white/5"
                      >
                        <span className="min-w-0">
                          <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                            {entry.number || t("未知号码")}
                          </span>
                          <span className="block text-xs text-gray-500 dark:text-gray-400">
                            {entry.direction === "incoming" ? t("来电") : t("呼出")} · {outcomeLabel(entry.outcome)} ·{" "}
                            {formatTime(entry.startedAt)}
                            {entry.durationSeconds > 0 ? ` · ${formatDuration(entry.durationSeconds)}` : ""}
                          </span>
                        </span>
                        <span className="shrink-0 text-xs text-[#0ea5e9]">{t("拨打")}</span>
                      </button>
                    ))}
                  </div>
                )
              ) : contactsLoading ? (
                <div className="py-6 text-center text-sm text-gray-400">{t("加载中...")}</div>
              ) : contacts.length === 0 ? (
                <div className="py-6 text-center text-sm text-gray-400">{t("暂无联系人")}</div>
              ) : (
                <div className="space-y-1">
                  {contacts.map((contact) => (
                    <button
                      key={`${contact.deviceId}-${contact.peer}`}
                      type="button"
                      onClick={() => setNumber(contact.peer)}
                      className="flex w-full items-center justify-between gap-3 rounded-lg px-3 py-2 text-left transition-colors hover:bg-black/5 dark:hover:bg-white/5"
                    >
                      <span className="min-w-0">
                        <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                          {contact.displayName || contact.peer}
                        </span>
                        {contact.displayName ? (
                          <span className="block truncate font-mono text-[10px] text-gray-400">{contact.peer}</span>
                        ) : null}
                      </span>
                      <span className="shrink-0 text-xs text-[#0ea5e9]">{t("拨打")}</span>
                    </button>
                  ))}
                </div>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
