import { api } from "../../api";

// VoWiFi 通话（IMS）相关接口封装。
// 说明：仅当设备 VoWiFi 已注册（IMSReady）且存在活跃通话时，通话媒体才可用。
// 底层契约见 internal/server/call_api.go 与 internal/server/call_media_api.go。

export type CallDirection = "incoming" | "outgoing";

// 与后端 ims 会话状态一致：dialing/ringing/early_media/active/ended/failed。
export type CallState =
  | "dialing"
  | "ringing"
  | "early_media"
  | "active"
  | "ended"
  | "failed";

export interface VoWiFiCall {
  id: string;
  number: string;
  direction: CallDirection;
  state: CallState;
  startedAt?: string;
  answeredAt?: string;
  sipCode?: number;
  reason?: string;
  mediaReady?: boolean;
  codec?: string;
  endedAt?: string;
}

export interface CallListResult {
  deviceId: string;
  // "vowifi" 表示通话走 IMS，可用网页音频；"cellular" 仅信令、无音频。
  transport: string;
  calls: VoWiFiCall[];
}

export interface CallActionResult {
  accepted: boolean;
  action: string;
  number?: string;
  callId?: string;
  durationSeconds?: number;
  transport: string;
  call?: VoWiFiCall | null;
}

export function listCalls(deviceId: string) {
  return api<CallListResult>(`/devices/${encodeURIComponent(deviceId)}/calls`);
}

export function dialCall(deviceId: string, number: string, durationSeconds = 0) {
  return api<CallActionResult>(`/devices/${encodeURIComponent(deviceId)}/calls/dial`, {
    method: "POST",
    body: { number, durationSeconds },
  });
}

export function answerCall(deviceId: string, callId: string) {
  return api<CallActionResult>(`/devices/${encodeURIComponent(deviceId)}/calls/answer`, {
    method: "POST",
    body: { callId },
  });
}

export function hangupCall(deviceId: string, callId: string) {
  return api<CallActionResult>(`/devices/${encodeURIComponent(deviceId)}/calls/hangup`, {
    method: "POST",
    body: { callId },
  });
}

export interface CallNumberValidation {
  valid: boolean;
}

// 与后端 validDialNumber 保持一致：2-32 位，允许数字、首位 +、*、#。
export function validateCallNumber(value: string): CallNumberValidation {
  const trimmed = value.trim();
  if (trimmed.length < 2 || trimmed.length > 32) return { valid: false };
  for (let index = 0; index < trimmed.length; index += 1) {
    const character = trimmed[index];
    const isDigit = character >= "0" && character <= "9";
    const isLeadingPlus = index === 0 && character === "+";
    const isStarOrHash = character === "*" || character === "#";
    if (!isDigit && !isLeadingPlus && !isStarOrHash) return { valid: false };
  }
  return { valid: true };
}
