import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowSyncRegular, CheckmarkCircleRegular, OpenRegular } from "@fluentui/react-icons";
import { Modal } from "../ui/Modal";
import { Button } from "../ui/Button";
import { Spinner } from "../ui/Spinner";
import { message } from "../ui/message";
import { useI18n } from "../../lib/i18n";
import { apiMessage } from "../../api";
import { readOIDCMessage } from "../../lib/oidcPopup";

interface OIDCFlowDialogProps {
  open: boolean;
  onClose: () => void;
  /** 由父组件在点击手势内同步打开的授权弹出窗口。 */
  popup: Window | null;
  /** 重新打开授权弹出窗口（必须在点击手势内调用，否则会被弹窗拦截）。 */
  onOpenPopup: () => Window | null;
  /** 请求一次性授权地址（绑定/登录各自的后端接口）。 */
  start: () => Promise<string>;
  /** 授权完成时回调页回传的成功消息类型。 */
  successEvent: string;
  /** 成功后对用户展示的提示。 */
  successMessage: string;
  /** 成功后的业务处理（刷新状态、跳转等）。 */
  onSuccess: () => void | Promise<void>;
  /** 弹窗标题。 */
  title: string;
  /** 后端未返回具体原因时的失败提示。 */
  errorFallback: string;
}

// 通用 OIDC 授权弹窗：负责把一次性授权地址写入居中弹出窗口、展示进度/错误、
// 并提供「重新打开授权页面」兜底。绑定与登录共用，区别只在 start/successEvent/onSuccess。
export function OIDCFlowDialog({
  open,
  onClose,
  popup,
  onOpenPopup,
  start,
  successEvent,
  successMessage,
  onSuccess,
  title,
  errorFallback,
}: OIDCFlowDialogProps) {
  const { t } = useI18n();
  const [loading, setLoading] = useState(false);
  const [opened, setOpened] = useState(false);
  const [error, setError] = useState("");
  // 授权成功：在站内弹窗内展示成功提示片刻，再执行后续处理并关闭（新窗口已自动关闭）。
  const [succeeded, setSucceeded] = useState(false);
  // 点击「重新打开授权页面」时自增，用于重新发起授权请求。
  const [reloadToken, setReloadToken] = useState(0);
  const closeTimer = useRef<number | null>(null);

  // 组件卸载时清理延时关闭定时器。
  useEffect(() => {
    return () => {
      if (closeTimer.current !== null) window.clearTimeout(closeTimer.current);
    };
  }, []);

  // 持有最新的弹出窗口引用，避免 effect 闭包拿到旧引用。
  const popupRef = useRef<Window | null>(popup);
  useEffect(() => {
    popupRef.current = popup;
  }, [popup]);

  // 用 ref 持有 start，使 launch 的函数身份保持稳定：
  // 否则父组件每次 re-render 传入新函数都会让发起 effect 反复触发、循环请求授权地址。
  const startRef = useRef(start);
  useEffect(() => {
    startRef.current = start;
  }, [start]);

  // 取一次性授权地址并写入弹出窗口；窗口被拦截时给出可操作提示。
  const launch = useCallback(
    (target: Window | null) => {
      // 重新发起时清理上一次的成功延时关闭，避免旧流程的后续处理再次触发。
      if (closeTimer.current !== null) {
        window.clearTimeout(closeTimer.current);
        closeTimer.current = null;
      }
      setSucceeded(false);
      if (!target) {
        setOpened(false);
        setLoading(false);
        setError(t("浏览器拦截了新窗口，请允许弹窗后重试"));
        return;
      }
      setError("");
      setLoading(true);
      startRef
        .current()
        .then((authorizationUrl) => {
          if (!authorizationUrl) {
            setOpened(false);
            setError(t("未获取到 OIDC 授权地址"));
            return;
          }
          // 弹出窗口初始为 about:blank，导航到授权地址后由 IDP 完成授权。
          target.location.href = authorizationUrl;
          setOpened(true);
        })
        .catch((reason: unknown) => {
          setOpened(false);
          setError(apiMessage(reason));
        })
        .finally(() => setLoading(false));
    },
    [t],
  );

  // 打开弹窗即发起授权：弹出窗口已在父组件的点击手势内打开。
  useEffect(() => {
    if (!open) return;
    setOpened(false);
    setError("");
    setSucceeded(false);
    launch(popupRef.current && !popupRef.current.closed ? popupRef.current : null);
  }, [open, reloadToken, launch]);

  // 监听回调页的 postMessage：成功自动收起弹窗并执行后续处理，失败给出原因。
  // 不按 open 开关监听：新窗口流程中用户可能已关闭弹窗，此时仍需更新状态。
  useEffect(() => {
    function onMessage(event: MessageEvent<unknown>) {
      // 只接受同源回调页的消息（合成事件没有 origin，一并放行），防止第三方伪造。
      if (event.origin && event.origin !== window.location.origin) return;
      const data = readOIDCMessage(event.data);
      if (!data) return;
      if (data.type === successEvent) {
        // 新窗口里的结果页会在 postMessage 后自行关闭；成功提示统一显示在站内弹窗内。
        setSucceeded(true);
        setOpened(false);
        setError("");
        message.success(successMessage);
        // 让用户看到弹窗内的成功提示后，再执行后续处理并关闭弹窗。
        if (closeTimer.current !== null) window.clearTimeout(closeTimer.current);
        closeTimer.current = window.setTimeout(() => {
          closeTimer.current = null;
          void Promise.resolve(onSuccess());
          onClose();
        }, 800);
      } else if (data.type === "vocat:oidc-error") {
        const reason = data.message || errorFallback;
        setOpened(false);
        setError(reason);
        message.error(reason);
      }
    }
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, [successEvent, successMessage, onSuccess, onClose, errorFallback, t]);

  // 重新打开：必须在点击手势内先 open，再取新的授权地址（旧地址的 state 已失效）。
  function reopen() {
    const target = onOpenPopup();
    if (!target) {
      message.warning(t("浏览器拦截了新窗口，请允许弹窗后重试"));
      return;
    }
    setReloadToken((value) => value + 1);
    launch(target);
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button
            icon={opened ? <ArrowSyncRegular /> : <OpenRegular />}
            loading={loading}
            disabled={succeeded}
            onClick={reopen}
          >
            {opened ? t("重新打开授权页面") : t("打开授权页面")}
          </Button>
          <Button variant="primary" onClick={onClose}>
            {t("关闭")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p className="text-sm text-gray-600 dark:text-gray-300">
          {t("已在新的浏览器窗口中打开 OIDC 登录页面，请在该窗口完成身份认证。完成后新窗口会自动关闭，结果会显示在这里。")}
        </p>
        <div className="flex min-h-[132px] flex-col items-center justify-center gap-3 rounded-xl border border-gray-200/70 bg-gray-50 px-4 py-6 text-center dark:border-white/10 dark:bg-white/5">
          {succeeded ? (
            <>
              <CheckmarkCircleRegular className="h-6 w-6 text-emerald-500" />
              <span className="text-sm font-semibold text-emerald-600 dark:text-emerald-400">
                {successMessage}
              </span>
            </>
          ) : loading ? (
            <>
              <Spinner className="h-5 w-5" />
              <span className="text-sm text-gray-500 dark:text-gray-400">
                {t("正在发起 OIDC 授权…")}
              </span>
            </>
          ) : error ? (
            <div className="w-full break-words rounded-xl border border-red-200/70 bg-red-50/80 px-4 py-3 text-sm text-red-600 dark:border-red-500/20 dark:bg-red-500/10 dark:text-red-300">
              {error}
            </div>
          ) : opened ? (
            <>
              <Spinner className="h-5 w-5" />
              <span className="text-sm text-gray-500 dark:text-gray-400">
                {t("授权页面已打开，请在新窗口完成登录/授权。")}
              </span>
            </>
          ) : null}
        </div>
        <p className="text-xs leading-5 text-gray-400 dark:text-gray-500">
          {t("若新窗口没有出现或被浏览器拦截，请点「重新打开授权页面」；浏览器提示允许弹窗后即可继续。")}
        </p>
      </div>
    </Modal>
  );
}
