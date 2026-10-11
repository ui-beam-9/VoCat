import { api } from "../api";

// 号码归属地：后端代理请求 uapis.cn 并缓存到数据库（同号只查一次上游）。
// 前端也维护一份内存缓存，避免重复请求。

export interface PhoneInfo {
  phone: string;
  province: string;
  city: string;
  sp: string;
  areaCode: string;
  display: string;
}

const cache = new Map<string, PhoneInfo>();
const BATCH_SIZE = 100;

function key(number: string): string {
  return number.trim();
}

export async function fetchPhoneInfo(phone: string): Promise<PhoneInfo> {
  const result = await api<PhoneInfo>(`/phone-info?phone=${encodeURIComponent(phone.trim())}`);
  cache.set(key(phone), result);
  return result;
}

// fetchPhoneLocations 批量解析号码归属地，返回 number -> PhoneInfo（未解析到的号码不含在结果里）。
// 结果在前端内存缓存，同一批号码重复调用不会再次请求。
export async function fetchPhoneLocations(numbers: string[]): Promise<Map<string, PhoneInfo>> {
  const unique = [...new Set(numbers.map((number) => number.trim()).filter(Boolean))];
  const result = new Map<string, PhoneInfo>();
  for (const number of unique) {
    const cached = cache.get(key(number));
    if (cached) result.set(key(number), cached);
  }
  const missing = unique.filter((number) => !cache.has(key(number)));
  for (let index = 0; index < missing.length; index += BATCH_SIZE) {
    const chunk = missing.slice(index, index + BATCH_SIZE);
    const query = chunk.map((number) => encodeURIComponent(number)).join(",");
    const payload = await api<{ items?: PhoneInfo[] }>(`/phone-info/batch?phones=${query}`);
    for (const item of payload.items ?? []) {
      if (item?.phone) cache.set(key(item.phone), item);
    }
  }
  for (const number of unique) {
    const cached = cache.get(key(number));
    if (cached) result.set(key(number), cached);
  }
  return result;
}
