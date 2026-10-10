// 本地联系人：从通话记录手动保存的号码，仅存浏览器本地（localStorage）。
// 与通讯录中的"短信联系人"合并展示。

export interface LocalContact {
  id: string;
  name: string;
  number: string;
  createdAt: string;
}

const STORAGE_KEY = "vocat.localContacts";
const MAX_ENTRIES = 500;

export function loadLocalContacts(): LocalContact[] {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as LocalContact[]) : [];
  } catch {
    return [];
  }
}

export function addLocalContact(contact: LocalContact): LocalContact[] {
  const next = [contact, ...loadLocalContacts()].slice(0, MAX_ENTRIES);
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  } catch {
    /* 存储不可用时静默忽略 */
  }
  return next;
}

export function newLocalContactId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}
