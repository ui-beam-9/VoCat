import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, apiMessage } from "../api";
import { useI18n } from "../lib/i18n";
import { Button, PageHeader, StatusDot, Tabs, confirmDialog, message } from "../components/ui";
import { DeviceCallPanel } from "../components/devices/DeviceCallPanel";
import { CallHistoryList } from "../components/devices/CallHistoryList";
import { CallContactsList } from "../components/devices/CallContactsList";
import { SaveContactDialog } from "../components/devices/SaveContactDialog";
import { cx } from "../lib/utils";
import {
  deleteContact,
  fetchContacts,
  saveContact,
  updateContact,
  type Contact,
} from "../lib/contacts";
import {
  clearCallHistory,
  deleteCallHistoryEntry,
  fetchCallHistory,
  type CallDirection,
  type CallHistoryEntry,
  type CallOutcome,
} from "../lib/callHistory";
import { fetchPhoneLocations } from "../lib/phoneInfo";
import type { DeviceListItem, DevicesResponse } from "../types";

const POLL_INTERVAL_MS = 5000;

// 全局"语音通话"页：设备选择 + 拨号盘 + 通话记录 + 通讯录。
// 音频依赖 VoWiFi（IMS 已注册）；通话记录与本地通讯录均存服务端。
export default function CallsPage() {
  const { t } = useI18n();
  const [searchParams] = useSearchParams();
  const [devices, setDevices] = useState<DeviceListItem[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [number, setNumber] = useState("");
  const [tab, setTab] = useState<"history" | "contacts">("history");
  const [history, setHistory] = useState<CallHistoryEntry[]>([]);
  const [savedContacts, setSavedContacts] = useState<Contact[]>([]);
  const [contactsLoading, setContactsLoading] = useState(false);
  const [deletingHistoryId, setDeletingHistoryId] = useState<number | null>(null);
  const [deletingContactId, setDeletingContactId] = useState<number | null>(null);
  const [saveTarget, setSaveTarget] = useState<CallHistoryEntry | null>(null);
  const [editTarget, setEditTarget] = useState<Contact | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  // ?device=<id> 只应用一次，之后用户在页面内切换设备不会被轮询覆盖。
  const requestedDeviceRef = useRef((searchParams.get("device") || "").trim());

  useEffect(() => {
    const requested = (searchParams.get("device") || "").trim();
    if (requested) requestedDeviceRef.current = requested;
  }, [searchParams]);

  const refreshDevices = useCallback(async () => {
    try {
      const res = await api<DevicesResponse>("/devices");
      // wifi_410 后端不支持通话，直接排除。
      const list = (res.devices ?? []).filter((device) => device.deviceType !== "wifi_410");
      setDevices(list);
      setError("");
      const requested = requestedDeviceRef.current;
      requestedDeviceRef.current = "";
      setSelectedId((current) => {
        const desired = requested || current;
        if (desired && list.some((device) => device.id === desired)) return desired;
        return current && list.some((device) => device.id === current) ? current : list[0]?.id ?? "";
      });
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
    setContactsLoading(true);
    try {
      setSavedContacts(await fetchContacts());
    } catch {
      setSavedContacts([]);
    } finally {
      setContactsLoading(false);
    }
  }, []);

  useEffect(() => {
    void refreshHistory();
    void refreshSavedContacts();
  }, [refreshHistory, refreshSavedContacts]);

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

  // 归属地：为通话记录与通讯录中的号码批量解析（后端同号只查一次并缓存到数据库）。
  const [locationByNumber, setLocationByNumber] = useState<Map<string, string>>(new Map());
  const locationNumbersKey = useMemo(
    () =>
      [...new Set(
        [...history.map((entry) => entry.number), ...savedContacts.map((contact) => contact.number)].filter(
          (number) => number.trim() !== "",
        ),
      )]
        .sort()
        .join(","),
    [history, savedContacts],
  );
  useEffect(() => {
    const numbers = locationNumbersKey ? locationNumbersKey.split(",") : [];
    if (numbers.length === 0) return;
    let cancelled = false;
    fetchPhoneLocations(numbers)
      .then((map) => {
        if (cancelled) return;
        setLocationByNumber(new Map([...map].map(([number, info]) => [number, info.display])));
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [locationNumbersKey]);

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

  const handleDeleteHistory = useCallback(
    async (entry: CallHistoryEntry) => {
      setDeletingHistoryId(entry.id);
      try {
        await deleteCallHistoryEntry(entry.id);
        setHistory((current) => current.filter((item) => item.id !== entry.id));
      } catch (err) {
        message.error(apiMessage(err));
      } finally {
        setDeletingHistoryId(null);
      }
    },
    [],
  );

  const handleDeleteContact = useCallback(
    async (contact: Contact) => {
      const ok = await confirmDialog(t("确定删除该联系人？"), t("删除联系人"), {
        confirmText: t("删除"),
        cancelText: t("取消"),
        type: "warning",
      });
      if (!ok) return;
      setDeletingContactId(contact.id);
      try {
        await deleteContact(contact.id);
        setSavedContacts((current) => current.filter((item) => item.id !== contact.id));
        message.success(t("联系人已删除"));
      } catch (err) {
        message.error(apiMessage(err));
      } finally {
        setDeletingContactId(null);
      }
    },
    [t],
  );

  const handleSaveNewContact = useCallback(
    async (name: string, num: string) => {
      try {
        await saveContact(name, num);
        setSaveTarget(null);
        await refreshSavedContacts();
        message.success(t("已保存为联系人"));
      } catch (err) {
        message.error(apiMessage(err));
      }
    },
    [refreshSavedContacts, t],
  );

  const handleSaveEditedContact = useCallback(
    async (name: string, num: string) => {
      if (!editTarget) return;
      try {
        await updateContact(editTarget.id, name, num);
        setEditTarget(null);
        await refreshSavedContacts();
        message.success(t("联系人已更新"));
      } catch (err) {
        message.error(apiMessage(err));
      }
    },
    [editTarget, refreshSavedContacts, t],
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
                  <Button size="small" loading={contactsLoading} onClick={() => void refreshSavedContacts()}>
                    {t("刷新")}
                  </Button>
                )}
              </div>
            </div>
            <div className="mt-4">
              {tab === "history" ? (
                <CallHistoryList
                  entries={history}
                  contactNameByNumber={contactNameByNumber}
                  locationByNumber={locationByNumber}
                  outcomeLabel={outcomeLabel}
                  onDial={setNumber}
                  onSave={setSaveTarget}
                  onDelete={(entry) => void handleDeleteHistory(entry)}
                  deletingId={deletingHistoryId}
                />
              ) : (
                <CallContactsList
                  contacts={savedContacts}
                  loading={contactsLoading}
                  locationByNumber={locationByNumber}
                  onDial={setNumber}
                  onEdit={setEditTarget}
                  onDelete={(contact) => void handleDeleteContact(contact)}
                  deletingId={deletingContactId}
                />
              )}
            </div>
          </div>
        </div>
      </div>
      <SaveContactDialog
        open={saveTarget !== null}
        initialNumber={saveTarget?.number ?? ""}
        onClose={() => setSaveTarget(null)}
        onSave={handleSaveNewContact}
      />
      <SaveContactDialog
        open={editTarget !== null}
        initialName={editTarget?.name ?? ""}
        initialNumber={editTarget?.number ?? ""}
        title={t("编辑联系人")}
        onClose={() => setEditTarget(null)}
        onSave={handleSaveEditedContact}
      />
    </div>
  );
}
