import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { ArrowRightRegular, LockClosedRegular, PersonRegular } from "@fluentui/react-icons";
import { useAuth } from "../store/auth";
import { useI18n } from "../lib/i18n";
import { message } from "../components/ui/message";
import { BrandLogo } from "../components/shell/BrandLogo";
import { OIDCIcon } from "../components/ui/OIDCIcon";
import { OIDCFlowDialog } from "../components/shell/OIDCFlowDialog";
import { getOIDCStatus, startOIDCLogin } from "../api";
import { OIDC_LOGIN_POPUP_NAME, openOIDCPopup } from "../lib/oidcPopup";
import type { OIDCStatus } from "../types";

const INPUT_CLASS =
  "w-full rounded-lg border border-gray-200 bg-white/70 py-3 pl-10 pr-4 font-mono text-sm text-gray-900 placeholder-gray-400 outline-none transition-all focus:border-indigo-500/40 focus:ring-2 focus:ring-indigo-500/25 dark:border-white/10 dark:bg-black/20 dark:text-gray-100 dark:placeholder-gray-500";

export default function LoginPage() {
  const { login, refresh } = useAuth();
  const { t } = useI18n();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [working, setWorking] = useState(false);
  const [oidcStatus, setOIDCStatus] = useState<OIDCStatus | null>(null);
  const [oidcDialogOpen, setOIDCDialogOpen] = useState(false);
  const [oidcPopup, setOIDCPopup] = useState<Window | null>(null);

  // 登录页未登录也要能渲染：OIDC 状态加载失败时静默忽略，只保留密码登录。
  const refreshOIDCStatus = useCallback(async () => {
    try {
      setOIDCStatus(await getOIDCStatus());
    } catch {
      // 静默忽略：例如后端尚未部署 OIDC 接口。
    }
  }, []);

  useEffect(() => {
    void refreshOIDCStatus();
    // 在其它标签页完成绑定/修改配置后，回到登录页无需手动刷新即可看到最新状态。
    const onFocus = () => {
      void refreshOIDCStatus();
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") void refreshOIDCStatus();
    };
    window.addEventListener("focus", onFocus);
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      window.removeEventListener("focus", onFocus);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [refreshOIDCStatus]);

  const currentRedirect = searchParams.get("redirect") || "/";

  // 登录页 OIDC 登录：在点击手势内同步打开居中授权窗口，再由弹窗写入一次性授权地址。
  function oidcLogin() {
    if (!oidcStatus?.enabled) return;
    setOIDCPopup(openOIDCPopup(OIDC_LOGIN_POPUP_NAME));
    setOIDCDialogOpen(true);
  }

  const openLoginPopup = useCallback(() => openOIDCPopup(OIDC_LOGIN_POPUP_NAME), []);

  // 取一次性授权地址：保持函数身份稳定，避免 OIDCFlowDialog 的发起 effect 反复触发。
  const startLoginPopup = useCallback(
    async () => (await startOIDCLogin(currentRedirect)).authorizationUrl,
    [currentRedirect],
  );

  // 弹窗授权成功后：刷新会话（LoginLayout 会按 ?redirect= 自动跳转），再补一次显式跳转兜底。
  const onOIDCLoggedIn = useCallback(async () => {
    await refresh();
    navigate(currentRedirect, { replace: true });
  }, [currentRedirect, navigate, refresh]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!username || !password) {
      message.warning(t("请输入用户名和密码"));
      return;
    }
    setWorking(true);
    await new Promise((resolve) => setTimeout(resolve, 600));
    const ok = await login(username, password);
    setWorking(false);
    if (ok) {
      message.success(t("欢迎回来"));
      const redirect = searchParams.get("redirect");
      navigate(redirect ? decodeURIComponent(redirect) : "/", { replace: true });
    } else {
      message.error(t("登录失败，请检查凭证"));
    }
  }

  return (
    <div className="relative flex h-full w-full items-center justify-center overflow-hidden">
      <div className="animate-pulse-slow absolute -left-32 -top-32 h-[520px] w-[520px] rounded-full bg-indigo-500/15 blur-[120px] dark:bg-indigo-500/20" />
      <div className="animate-pulse-slow absolute -bottom-32 -right-32 h-[520px] w-[520px] rounded-full bg-indigo-500/12 blur-[120px] [animation-delay:2s] dark:bg-indigo-500/16" />
      <div className="relative w-full max-w-md p-1">
        <div className="group relative overflow-hidden rounded-2xl border border-gray-100 bg-white/70 p-8 shadow-2xl backdrop-blur-xl dark:border-white/10 dark:bg-[#141418]/70">
          <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-indigo-500/8 to-transparent opacity-0 transition-opacity duration-500 group-hover:opacity-100" />
          <div className="relative z-10 mb-10 text-center">
            <div className="mx-auto mb-6 flex h-20 w-20 items-center justify-center rounded-2xl bg-white shadow-lg shadow-indigo-500/20 ring-1 ring-black/5 transition-transform duration-300 group-hover:scale-105 dark:bg-white/10 dark:ring-white/10">
              <BrandLogo className="h-14 w-14" />
            </div>
            <h2 className="bg-gradient-to-r from-gray-900 to-gray-600 bg-clip-text text-3xl font-bold text-transparent dark:from-white dark:to-gray-400">
              vocat
            </h2>
            <p className="mt-3 text-sm tracking-wide text-gray-500 dark:text-gray-400">{t("高通模块专业测试工具")}</p>
          </div>
          <form onSubmit={submit} className="relative z-10 space-y-6">
            <div className="space-y-2">
              <div className="relative">
                <div className="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3 text-gray-400 dark:text-gray-500">
                  <PersonRegular className="h-5 w-5" />
                </div>
                <input
                  className={INPUT_CLASS}
                  placeholder={t("用户名")}
                  type="text"
                  autoComplete="username"
                  value={username}
                  onChange={(event) => setUsername(event.target.value)}
                />
              </div>
            </div>
            <div className="space-y-2">
              <div className="relative">
                <div className="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3 text-gray-400 dark:text-gray-500">
                  <LockClosedRegular className="h-5 w-5" />
                </div>
                <input
                  className={INPUT_CLASS}
                  placeholder={t("密码")}
                  type="password"
                  autoComplete="current-password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                />
              </div>
            </div>
            <button
              type="submit"
              disabled={working}
              className="flex w-full items-center justify-center gap-2 rounded-lg bg-[#0ea5e9] px-4 py-3 font-bold text-white shadow-sm transition-all duration-200 hover:bg-[#0284c7] active:scale-95 disabled:cursor-not-allowed disabled:opacity-70"
            >
              {working ? (
                <span className="h-5 w-5 animate-spin rounded-full border-2 border-white/30 border-t-white" />
              ) : (
                <span>{t("登录")}</span>
              )}
              {!working && <ArrowRightRegular className="h-5 w-5" />}
            </button>
          </form>
          {/* 只要 OIDC 已启用就显示入口；未绑定账号在弹窗内会得到「未绑定」提示。 */}
          {oidcStatus?.enabled && (
            <div className="relative z-10 mt-6">
              <div className="mb-4 flex items-center gap-3 text-xs text-gray-400 dark:text-gray-500">
                <span className="h-px flex-1 bg-gray-200 dark:bg-white/10" />
                <span>{t("或")}</span>
                <span className="h-px flex-1 bg-gray-200 dark:bg-white/10" />
              </div>
              <button
                type="button"
                onClick={oidcLogin}
                className="flex w-full items-center justify-center gap-2 rounded-lg border border-gray-200 bg-white/70 px-4 py-3 text-sm font-semibold text-gray-700 shadow-sm transition-all duration-200 hover:border-indigo-400 hover:text-indigo-600 active:scale-95 dark:border-white/10 dark:bg-black/20 dark:text-gray-200 dark:hover:border-indigo-400/60 dark:hover:text-indigo-300"
              >
                <OIDCIcon className="h-5 w-5" />
                <span>{oidcStatus.loginButtonText || t("OIDC 登录")}</span>
              </button>
            </div>
          )}
        </div>
        <div className="mt-6 text-center">
          <p className="text-xs text-gray-500">vocat © 2026</p>
        </div>
      </div>
      <OIDCFlowDialog
        open={oidcDialogOpen}
        onClose={() => setOIDCDialogOpen(false)}
        popup={oidcPopup}
        onOpenPopup={openLoginPopup}
        start={startLoginPopup}
        successEvent="vocat:oidc-login"
        successMessage={t("OIDC 登录成功")}
        errorFallback={t("OIDC 登录失败")}
        onSuccess={onOIDCLoggedIn}
        title={t("OIDC 登录")}
      />
    </div>
  );
}
