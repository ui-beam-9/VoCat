import { api } from "../api";

// 号码归属地：后端代理请求 uapis.cn，前端只消费 /api/phone-info。
// 查询结果在内存中缓存，避免同一次通话重复请求。

export interface PhoneInfo {
  phone: string;
  province: string;
  city: string;
  sp: string;
  areaCode: string;
  display: string;
}

const cache = new Map<string, PhoneInfo>();

export async function fetchPhoneInfo(phone: string): Promise<PhoneInfo> {
  const key = phone.trim();
  const cached = cache.get(key);
  if (cached) return cached;
  const result = await api<PhoneInfo>(`/phone-info?phone=${encodeURIComponent(key)}`);
  cache.set(key, result);
  return result;
}
