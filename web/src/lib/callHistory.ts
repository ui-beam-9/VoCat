// 通话记录：仅保存在浏览器本地（localStorage），记录通过本页面发起的通话。
// 说明：按需求采用前端本地保存；不含其他浏览器/其他客户端发起的通话，清缓存会丢失。

export type CallDirection = "incoming" | "outgoing";
export type CallOutcome = "answered" | "missed" | "failed";

export interface CallHistoryEntry {
  id: string;
  deviceId: string;
  number: string;
  direction: CallDirection;
  outcome: CallOutcome;
  startedAt: string;
  endedAt: string;
  durationSeconds: number;
}

export type NewCallHistoryEntry = Omit<CallHistoryEntry, "id" | "deviceId">;

const STORAGE_KEY = "vocat.callHistory";
const MAX_ENTRIES = 200;

export function loadCallHistory(): CallHistoryEntry[] {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as CallHistoryEntry[]) : [];
  } catch {
    return [];
  }
}

export function appendCallHistory(entry: CallHistoryEntry): CallHistoryEntry[] {
  const next = [entry, ...loadCallHistory()].slice(0, MAX_ENTRIES);
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  } catch {
    /* 存储不可用时静默忽略 */
  }
  return next;
}

export function clearCallHistory(): CallHistoryEntry[] {
  try {
    window.localStorage.removeItem(STORAGE_KEY);
  } catch {
    /* ignore */
  }
  return [];
}

export function newCallHistoryId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}
