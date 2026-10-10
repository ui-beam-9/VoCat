import { api } from "../api";

// 通话记录（服务端数据库存储）。列表/新增/清空均走 /api/call-history。

export type CallDirection = "incoming" | "outgoing";
export type CallOutcome = "answered" | "missed" | "failed";

export interface CallHistoryEntry {
  id: number;
  callId: string;
  deviceId: string;
  number: string;
  direction: CallDirection;
  outcome: CallOutcome;
  startedAt: string;
  endedAt: string;
  durationSeconds: number;
}

export type NewCallHistoryEntry = Omit<CallHistoryEntry, "id" | "deviceId">;

export async function fetchCallHistory(deviceId?: string): Promise<CallHistoryEntry[]> {
  const params = new URLSearchParams({ limit: "200" });
  if (deviceId) params.set("device_id", deviceId);
  const result = await api<{ entries?: CallHistoryEntry[] }>(`/call-history?${params.toString()}`);
  return result.entries ?? [];
}

export async function createCallHistoryEntry(
  deviceId: string,
  entry: NewCallHistoryEntry,
): Promise<CallHistoryEntry> {
  const result = await api<{ entry: CallHistoryEntry }>("/call-history", {
    method: "POST",
    body: {
      callId: entry.callId,
      deviceId,
      number: entry.number,
      direction: entry.direction,
      outcome: entry.outcome,
      startedAt: entry.startedAt,
      endedAt: entry.endedAt,
      durationSeconds: entry.durationSeconds,
    },
  });
  return result.entry;
}

export async function clearCallHistory(deviceId?: string): Promise<void> {
  const suffix = deviceId ? `?device_id=${encodeURIComponent(deviceId)}` : "";
  await api(`/call-history${suffix}`, { method: "DELETE" });
}
