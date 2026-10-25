// OIDC 授权弹窗的公共工具：居中打开命名弹出窗口、解析回调页的 postMessage 载荷。
// 绑定与登录两条流程共用，保证「点按钮 → 弹窗授权」的体验一致。

// 绑定与登录使用不同的窗口名，避免两条流程互相复用同一个窗口。
export const OIDC_BIND_POPUP_NAME = "vocat-oidc-bind";
export const OIDC_LOGIN_POPUP_NAME = "vocat-oidc-login";

export const OIDC_POPUP_WIDTH = 520;
export const OIDC_POPUP_HEIGHT = 760;

// 回调页通过 window.postMessage 回传的消息结构（见后端契约）：
// data.type 为 "vocat:oidc-bound" / "vocat:oidc-login" / "vocat:oidc-error"。
export interface OIDCMessageData {
  type: string;
  message?: string;
}

// 从任意 postMessage 载荷中安全提取消息，避免使用 any。
export function readOIDCMessage(payload: unknown): OIDCMessageData | null {
  if (typeof payload !== "object" || payload === null) return null;
  const record = payload as Record<string, unknown>;
  if (typeof record.type !== "string") return null;
  return {
    type: record.type,
    message: typeof record.message === "string" ? record.message : undefined,
  };
}

// 计算授权弹窗的居中位置：相对当前浏览器窗口居中，并限制在屏幕可视区域内。
function centeredPopupFeatures(): string {
  const parentLeft = typeof window.screenX === "number" ? window.screenX : window.screenLeft;
  const parentTop = typeof window.screenY === "number" ? window.screenY : window.screenTop;
  const parentWidth =
    window.outerWidth || document.documentElement.clientWidth || OIDC_POPUP_WIDTH;
  const parentHeight =
    window.outerHeight || document.documentElement.clientHeight || OIDC_POPUP_HEIGHT;
  const left = Math.max(0, Math.round(parentLeft + (parentWidth - OIDC_POPUP_WIDTH) / 2));
  const top = Math.max(0, Math.round(parentTop + (parentHeight - OIDC_POPUP_HEIGHT) / 2));
  return [
    "popup=yes",
    `width=${OIDC_POPUP_WIDTH}`,
    `height=${OIDC_POPUP_HEIGHT}`,
    `left=${left}`,
    `top=${top}`,
    "menubar=no",
    "toolbar=no",
    "location=no",
    "status=no",
    "resizable=yes",
    "scrollbars=yes",
  ].join(",");
}

// 打开（或复用）命名授权弹出窗口；必须在点击手势内同步调用，否则会被浏览器弹窗拦截。
export function openOIDCPopup(name: string): Window | null {
  return window.open("", name, centeredPopupFeatures());
}
