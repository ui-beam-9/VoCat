import { useCallback, useEffect, useMemo, useState } from "react";
import { api, apiMessage } from "../api";
import { useI18n } from "../lib/i18n";
import { Button, PageHeader, StatusDot, Tabs, message } from "../components/ui";
import { DeviceCallPanel } from "../components/devices/DeviceCallPanel";
import { SaveContactDialog } from "../components/devices/SaveContactDialog";
import { listContacts } from "../components/sms/smsApi";
import { cx } from "../lib/utils";
import { fetchContacts, saveContact, type Contact } from "../lib/contacts";
import {
  clearCallHistory,
  fetchCallHistory,
  type CallDirection,
  type CallHistoryEntry,
  type CallOutcome,
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
  const [history, setHistory] = useState<CallHistoryEntry[]>([]);
  const [savedContacts, setSavedContacts] = useState<Contact[]>([]);
  const [saveTarget, setSaveTarget] = useState<CallHistoryEntry | null>(null);
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

  const refreshHistory = useCallback(async () => {
    try {
      setHistory(await fetchCallHistory());
    } catch (err) {
      setError(apiMessage(err));
    }
  }, []);

  const refreshSavedContacts = useCallback(async () => {
    try {
      setSavedContacts(await fetchContacts());
    } catch {
      setSavedContacts([]);
    }
  }, []);

  useEffect(() => {
    void refreshHistory();
    void refreshSavedContacts();
  }, [refreshHistory, refreshSavedContacts]);

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

  const outcomeLabel = useCallback(
    (outcome: CallOutcome, direction: CallDirection) => {
      if (outcome === "answered") return t("已接通");
      if (outcome === "failed") return t("呼叫失败");
      return direction === "incoming" ? t("未接来电") : t("未接通");
    },
    [t],
  );

  const contactNameByNumber = useMemo(() => {
    const map = new Map<string, string>();
    for (const contact of savedContacts) map.set(contact.number, contact.name);
    return map;
  }, [savedContacts]);

  // 通话记录由服务端实时写入，这里定期刷新以便近实时显示。
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void refreshHistory();
    }, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [refreshHistory]);

  const handleClearHistory = useCallback(async () => {
    try {
      await clearCallHistory();
      setHistory([]);
    } catch (err) {
      message.error(apiMessage(err));
    }
  }, []);

  const handleSaveContact = useCallback(
    async (name: string, number: string) => {
      try {
        await saveContact(name, number);
        setSaveTarget(null);
        await refreshSavedContacts();
        message.success(t("已保存为联系人"));
      } catch (err) {
        message.error(apiMessage(err));
      }
    },
    [refreshSavedContacts, t],
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
                    onClick={() => void handleClearHistory()}
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
                      <div
                        key={entry.id}
                        className="flex items-center justify-between gap-3 rounded-lg px-3 py-2 transition-colors hover:bg-black/5 dark:hover:bg-white/5"
                      >
                        <button
                          type="button"
                          onClick={() => setNumber(entry.number)}
                          className="min-w-0 flex-1 text-left"
                        >
                          {contactNameByNumber.has(entry.number) ? (
                            <>
                              <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                                {contactNameByNumber.get(entry.number)}
                              </span>
                              <span className="block truncate font-mono text-[10px] text-gray-400">{entry.number}</span>
                            </>
                          ) : (
                            <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                              {entry.number || t("未知号码")}
                            </span>
                          )}
                          <span className="block text-xs text-gray-500 dark:text-gray-400">
                            {entry.direction === "incoming" ? t("来电") : t("呼出")} ·{" "}
                            {outcomeLabel(entry.outcome, entry.direction)} · {formatTime(entry.startedAt)}
                            {entry.durationSeconds > 0 ? ` · ${formatDuration(entry.durationSeconds)}` : ""}
                          </span>
                        </button>
                        <div className="flex shrink-0 items-center gap-3 text-xs">
                          <button type="button" className="text-[#0ea5e9]" onClick={() => setNumber(entry.number)}>
                            {t("拨打")}
                          </button>
                          <button
                            type="button"
                            className="text-gray-500 transition-colors hover:text-gray-800 dark:text-gray-400 dark:hover:text-gray-200"
                            onClick={() => setSaveTarget(entry)}
                          >
                            {t("保存")}
                          </button>
                        </div>
                      </div>
                    ))}
                  </div>
                )
              ) : savedContacts.length === 0 && contacts.length === 0 ? (
                <div className="py-6 text-center text-sm text-gray-400">
                  {contactsLoading ? t("加载中...") : t("暂无联系人")}
                </div>
              ) : (
                <div className="space-y-1">
                  {savedContacts.map((contact) => (
                    <button
                      key={contact.id}
                      type="button"
                      onClick={() => setNumber(contact.number)}
                      className="flex w-full items-center justify-between gap-3 rounded-lg px-3 py-2 text-left transition-colors hover:bg-black/5 dark:hover:bg-white/5"
                    >
                      <span className="min-w-0">
                        <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                          {contact.name}
                        </span>
                        <span className="block truncate font-mono text-[10px] text-gray-400">{contact.number}</span>
                      </span>
                      <span className="flex shrink-0 items-center gap-2">
                        <span className="rounded bg-emerald-50 px-1.5 py-0.5 text-[10px] text-emerald-600 dark:bg-emerald-500/10 dark:text-emerald-300">
                          {t("本地")}
                        </span>
                        <span className="text-xs text-[#0ea5e9]">{t("拨打")}</span>
                      </span>
                    </button>
                  ))}
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
      <SaveContactDialog
        open={saveTarget !== null}
        initialNumber={saveTarget?.number ?? ""}
        onClose={() => setSaveTarget(null)}
        onSave={handleSaveContact}
      />
    </div>
  );
}
