// OIDC 状态变更事件：绑定/解绑/保存配置后广播，让设置卡片与左下角用户菜单双向实时刷新。
// 带来源标识，订阅方可忽略自己发出的事件，避免重复请求与相互触发。

export type OIDCChangeSource = "settings-card" | "shell";

const OIDC_CHANGE_EVENT = "vocat:oidc-changed";

interface OIDCChangeDetail {
  source?: OIDCChangeSource;
}

// 广播 OIDC 状态已变化（绑定/解绑/启用开关保存后调用）。
export function notifyOIDCChanged(source: OIDCChangeSource): void {
  window.dispatchEvent(new CustomEvent<OIDCChangeDetail>(OIDC_CHANGE_EVENT, { detail: { source } }));
}

// 订阅 OIDC 状态变化，返回取消订阅函数。
// ignoreSource 用于忽略自己发出的事件（例如设置卡片保存后已在本地回填，无需再拉一次）。
export function onOIDCChanged(handler: () => void, ignoreSource?: OIDCChangeSource): () => void {
  const listener = (event: Event) => {
    const detail = (event as CustomEvent<OIDCChangeDetail>).detail;
    if (ignoreSource && detail?.source === ignoreSource) return;
    handler();
  };
  window.addEventListener(OIDC_CHANGE_EVENT, listener);
  return () => window.removeEventListener(OIDC_CHANGE_EVENT, listener);
}
