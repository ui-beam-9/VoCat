import { api } from "../api";

// 通话记录（服务端数据库存储）。通话记录由服务端实时写入，前端只读/清空。

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

export async function fetchCallHistory(deviceId?: string): Promise<CallHistoryEntry[]> {
  const params = new URLSearchParams({ limit: "200" });
  if (deviceId) params.set("device_id", deviceId);
  const result = await api<{ entries?: CallHistoryEntry[] }>(`/call-history?${params.toString()}`);
  return result.entries ?? [];
}

export async function clearCallHistory(deviceId?: string): Promise<void> {
  const suffix = deviceId ? `?device_id=${encodeURIComponent(deviceId)}` : "";
  await api(`/call-history${suffix}`, { method: "DELETE" });
}
