import { useEffect, useState } from "react";
import { ArrowSyncRegular, OpenRegular } from "@fluentui/react-icons";
import { Modal } from "../ui/Modal";
import { Button } from "../ui/Button";
import { Spinner } from "../ui/Spinner";
import { message } from "../ui/message";
import { useI18n } from "../../lib/i18n";
import { apiMessage, startOIDCBinding } from "../../api";

interface OIDCBindDialogProps {
  open: boolean;
  onClose: () => void;
  /** 绑定成功后的回调：父组件用它重新拉取绑定状态。 */
  onBound: () => void;
}

// OIDC 回调页通过 window.postMessage 回传的消息结构（见后端契约：
// data.type 固定为 "vocat:oidc-bound" / "vocat:oidc-error"）。
interface OIDCMessageData {
  type: string;
  message?: string;
}

// 从任意 postMessage 载荷中安全提取消息，避免使用 any。
function readMessageData(payload: unknown): OIDCMessageData | null {
  if (typeof payload !== "object" || payload === null) return null;
  const record = payload as Record<string, unknown>;
  if (typeof record.type !== "string") return null;
  return {
    type: record.type,
    message: typeof record.message === "string" ? record.message : undefined,
  };
}

export function OIDCBindDialog({ open, onClose, onBound }: OIDCBindDialogProps) {
  const { t } = useI18n();
  const [authorizationUrl, setAuthorizationUrl] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [opening, setOpening] = useState(false);
  // 点击「重试」时自增，用于重新发起绑定请求。
  const [reloadToken, setReloadToken] = useState(0);

  // 每次打开弹窗都重新发起绑定：授权地址带一次性 state，不能复用旧的。
  useEffect(() => {
    if (!open) return;
    let active = true;
    setAuthorizationUrl("");
    setError("");
    setLoading(true);
    startOIDCBinding()
      .then((result) => {
        if (!active) return;
        if (!result.authorizationUrl) {
          setError(t("未获取到 OIDC 授权地址"));
          return;
        }
        setAuthorizationUrl(result.authorizationUrl);
      })
      .catch((reason: unknown) => {
        if (active) setError(apiMessage(reason));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [open, reloadToken, t]);

  // 监听回调页的 postMessage：绑定成功自动收起弹窗并刷新状态，失败给出原因。
  // 不按 open 开关监听：兜底的新窗口流程中用户可能已关闭弹窗，此时仍需刷新绑定状态。
  useEffect(() => {
    function onMessage(event: MessageEvent<unknown>) {
      // 只接受同源回调页的消息（合成事件没有 origin，一并放行），防止第三方伪造。
      if (event.origin && event.origin !== window.location.origin) return;
      const data = readMessageData(event.data);
      if (!data) return;
      if (data.type === "vocat:oidc-bound") {
        message.success(t("OIDC 用户绑定成功"));
        onBound();
        onClose();
      } else if (data.type === "vocat:oidc-error") {
        const reason = data.message || t("OIDC 用户绑定失败");
        setAuthorizationUrl("");
        setError(reason);
        message.error(reason);
      }
    }
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, [onBound, onClose, t]);

  // 兜底：身份提供方不允许嵌入 iframe 时，在新窗口完成授权。
  async function openInNewWindow() {
    // 先同步打开空白窗口，避免 await 之后被浏览器判定为非用户触发的弹窗。
    const popup = window.open("", "_blank");
    if (!popup) {
      message.warning(t("浏览器拦截了新窗口，请允许弹窗后重试"));
      return;
    }
    setOpening(true);
    try {
      const result = await startOIDCBinding();
      if (!result.authorizationUrl) {
        popup.close();
        message.error(t("未获取到 OIDC 授权地址"));
        return;
      }
      popup.location.href = result.authorizationUrl;
      message.info(t("已在新窗口打开授权页面，完成后会自动完成绑定"));
    } catch (reason) {
      popup.close();
      message.error(apiMessage(reason));
    } finally {
      setOpening(false);
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t("绑定 OIDC 用户")}
      footer={
        <>
          <Button icon={<OpenRegular />} loading={opening} onClick={() => { void openInNewWindow(); }}>
            {t("在新窗口打开授权页面")}
          </Button>
          <Button variant="primary" onClick={onClose}>
            {t("关闭")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p className="text-sm text-gray-600 dark:text-gray-300">
          {t("请在下方完成 OIDC 身份认证，授权成功后将自动完成绑定。")}
        </p>
        <div className="relative h-[min(520px,58vh)] w-full overflow-hidden rounded-xl border border-gray-200/70 bg-white dark:border-white/10 dark:bg-black/20">
          {authorizationUrl && !error && (
            <iframe
              title={t("OIDC 授权页面")}
              src={authorizationUrl}
              className="h-full w-full border-0"
            />
          )}
          {loading && (
            <div className="absolute inset-0 flex items-center justify-center gap-2 bg-white/80 text-sm text-gray-500 backdrop-blur-sm dark:bg-black/50 dark:text-gray-400">
              <Spinner className="h-4 w-4" />
              <span>{t("正在加载 OIDC 授权页面...")}</span>
            </div>
          )}
          {error && !loading && (
            <div className="absolute inset-0 flex flex-col items-center justify-center gap-4 px-6 text-center">
              <div className="w-full rounded-xl border border-red-200/70 bg-red-50/80 px-4 py-3 text-sm text-red-600 dark:border-red-500/20 dark:bg-red-500/10 dark:text-red-300">
                {error}
              </div>
              <Button icon={<ArrowSyncRegular />} onClick={() => setReloadToken((value) => value + 1)}>
                {t("重试")}
              </Button>
            </div>
          )}
        </div>
        <p className="text-xs text-gray-400 dark:text-gray-500">
          {t("授权完成后弹窗会自动关闭。若此处页面无法显示（身份提供方可能禁止嵌入），请使用「在新窗口打开授权页面」。")}
        </p>
      </div>
    </Modal>
  );
}
