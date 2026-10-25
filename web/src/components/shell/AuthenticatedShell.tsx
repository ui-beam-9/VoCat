import { useCallback, useEffect, useRef, useState } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import {
  BoardRegular,
  CallRegular,
  DocumentTextRegular,
  GlobeRegular,
  MailRegular,
  SendClockRegular,
  PanelLeftContractRegular,
  PanelLeftExpandRegular,
  RouterRegular,
  SettingsRegular,
  SignOutRegular,
} from "@fluentui/react-icons";
import { useAuth } from "../../store/auth";
import { useI18n } from "../../lib/i18n";
import { confirmDialog } from "../ui/MessageBox";
import { message } from "../ui/message";
import { LanguageSwitch } from "../ui/LanguageSwitch";
import { SwitchDark } from "../ui/SwitchDark";
import { Drawer } from "../ui/Drawer";
import { ErrorBoundary } from "../ui/ErrorBoundary";
import { OIDCIcon } from "../ui/OIDCIcon";
import { cx } from "../../lib/utils";
import { BrandLogo } from "./BrandLogo";
import { OIDCBindDialog } from "./OIDCBindDialog";
import { VersionBadge } from "./VersionBadge";
import { IncomingCallWatcher } from "../IncomingCallWatcher";
import { listPlugins, type InstalledPlugin } from "../../extensions";
import { api, apiMessage, deleteOIDCBinding, getOIDCStatus } from "../../api";
import type { OIDCStatus, SystemInfo } from "../../types";

// 用户菜单项样式：普通项与退出项（退出项使用红色悬停语义）。
const USER_MENU_ITEM_CLASS =
  "flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm text-gray-700 transition-colors hover:bg-black/5 disabled:cursor-not-allowed disabled:opacity-60 dark:text-gray-200 dark:hover:bg-white/10";
const USER_MENU_LOGOUT_CLASS =
  "flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm text-red-500 transition-colors hover:bg-red-50 disabled:cursor-not-allowed disabled:opacity-60 dark:text-red-400 dark:hover:bg-red-500/10";

const NAV = [
  { to: "/", label: "仪表盘", icon: BoardRegular, end: true },
  { to: "/devices", label: "设备管理", icon: RouterRegular },
  { to: "/proxy", label: "代理管理", icon: GlobeRegular },
  { to: "/sms", label: "短信检测", icon: MailRegular },
  { to: "/calls", label: "语音通话", icon: CallRegular },
  { to: "/automatic-tasks", label: "自动任务", icon: SendClockRegular },
  { to: "/logs", label: "实时日志", icon: DocumentTextRegular },
  { to: "/settings", label: "系统设置", icon: SettingsRegular },
];

export function AuthenticatedShell({
  isDark,
  onToggleTheme,
}: {
  isDark: boolean;
  onToggleTheme: () => void;
}) {
  const [collapsed, setCollapsed] = useState(false);
  const [isMobile, setIsMobile] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [plugins, setPlugins] = useState<InstalledPlugin[]>([]);
  const [developer, setDeveloper] = useState(false);
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const [bindDialogOpen, setBindDialogOpen] = useState(false);
  const [oidcStatus, setOIDCStatus] = useState<OIDCStatus | null>(null);
  const [oidcBusy, setOIDCBusy] = useState(false);
  const userMenuRef = useRef<HTMLDivElement>(null);
  const { logout, user } = useAuth();
  const { t, lang } = useI18n();
  const navigate = useNavigate();
  const location = useLocation();

  // 登录后加载一次 OIDC 状态：只在已启用且配置完整时显示绑定/解绑入口。
  const loadOIDCStatus = useCallback(async () => {
    try {
      setOIDCStatus(await getOIDCStatus());
    } catch {
      // 接口不可用或未启用时静默忽略，不影响侧栏其它功能。
      setOIDCStatus(null);
    }
  }, []);

  useEffect(() => {
    void loadOIDCStatus();
  }, [loadOIDCStatus]);

  const closeBindDialog = useCallback(() => setBindDialogOpen(false), []);
  const onBoundOIDC = useCallback(() => {
    void loadOIDCStatus();
  }, [loadOIDCStatus]);

  // 点击菜单外部或按 Esc 关闭用户菜单。
  useEffect(() => {
    if (!userMenuOpen) return;
    function onPointerDown(event: MouseEvent) {
      if (userMenuRef.current?.contains(event.target as Node)) return;
      setUserMenuOpen(false);
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") setUserMenuOpen(false);
    }
    document.addEventListener("mousedown", onPointerDown);
    window.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [userMenuOpen]);

  useEffect(() => {
    let active = true;
    const load = () => api<SystemInfo>("/system/info").then((info) => {
      if (active) setDeveloper(!!info.developer);
    }).catch(() => {
      if (active) setDeveloper(false);
    });
    void load();
    const timer = window.setInterval(load, 10_000);
    return () => { active = false; window.clearInterval(timer); };
  }, []);

  useEffect(() => {
    const mq = window.matchMedia("(max-width: 767px)");
    let last = mq.matches;
    const update = () => {
      const next = mq.matches;
      setIsMobile(next);
      // 桌面/移动断点切换时关掉用户菜单，避免状态残留导致菜单自己展开。
      if (next !== last) setUserMenuOpen(false);
      last = next;
      if (!next) setMobileOpen(false);
    };
    update();
    window.addEventListener("resize", update, { passive: true });
    return () => window.removeEventListener("resize", update);
  }, []);

  useEffect(() => {
    let active = true;
    const load = () => listPlugins().then((items) => {
      if (active) setPlugins(items || []);
    }).catch(() => undefined);
    void load();
    window.addEventListener("vocat:plugins-changed", load);
    return () => { active = false; window.removeEventListener("vocat:plugins-changed", load); };
  }, []);

  useEffect(() => {
    setMobileOpen(false);
    setUserMenuOpen(false);
  }, [location.pathname]);

  async function onLogout() {
    const ok = await confirmDialog(t("确认退出登录？"), t("提示"), {
      confirmText: t("退出"),
      cancelText: t("取消"),
      type: "warning",
    });
    if (ok) {
      await logout();
      navigate("/login");
    }
  }

  function openBindDialog() {
    setUserMenuOpen(false);
    setBindDialogOpen(true);
  }

  // 解绑前二次确认，失败时提示后端返回的原因。
  async function onUnbindOIDC() {
    setUserMenuOpen(false);
    const ok = await confirmDialog(
      t("解绑后将无法使用该 OIDC 身份登录，确定继续吗？"),
      t("解绑 OIDC 用户"),
      { confirmText: t("解绑"), cancelText: t("取消"), confirmVariant: "danger" },
    );
    if (!ok) return;
    setOIDCBusy(true);
    try {
      await deleteOIDCBinding();
      message.success(t("OIDC 用户已解绑"));
    } catch (error) {
      message.error(apiMessage(error) || t("解绑 OIDC 用户失败"));
    } finally {
      setOIDCBusy(false);
      void loadOIDCStatus();
    }
  }

  function toggle() {
    if (isMobile) setMobileOpen(true);
    else setCollapsed((value) => !value);
  }

  function menuList(collapse: boolean) {
    const sidebarPlugins = plugins
      .filter((plugin) => plugin.enabled)
      .flatMap((plugin) => plugin.contributions
        .filter((contribution) => contribution.location === "sidebar")
        .map((contribution) => ({ plugin, contribution })));
    const navItems: Array<(typeof NAV)[number] | { to: string; label: string; icon: typeof GlobeRegular; pluginLabelZH?: string }> = [];
    for (const item of NAV) {
      navItems.push(item);
	  if (developer && item.to === "/proxy") {
		navItems.push({ to: "/export-proxy", label: "导出代理", icon: GlobeRegular });
	  }
	  const itemKey = item.to.replace(/^\//, "") || "dashboard";
      for (const extension of sidebarPlugins.filter((entry) => (entry.contribution.after || "sms") === itemKey)) {
        navItems.push({
          to: `/extensions/${encodeURIComponent(extension.plugin.id)}/${encodeURIComponent(extension.contribution.id)}`,
          label: extension.contribution.label,
          pluginLabelZH: extension.contribution.labelZh,
          icon: GlobeRegular,
        });
      }
    }
    return (
      <nav className={cx("sidebar-menu mt-2", collapse && "is-collapsed")} aria-label={t("主导航")}>
        {navItems.map((item) => {
          const Icon = item.icon;
          const label = "pluginLabelZH" in item && item.pluginLabelZH
            ? (lang === "zh" ? item.pluginLabelZH : item.label)
            : t(item.label);
          return (
            <NavLink
              key={item.to}
              to={item.to}
              end={"end" in item ? item.end : undefined}
              title={collapse ? label : undefined}
              className={({ isActive }) => cx("vocat-menu-item", isActive && "is-active")}
            >
              <span className="vocat-menu-icon">
                <Icon />
              </span>
              <span className="sidebar-menu-label">{label}</span>
            </NavLink>
          );
        })}
      </nav>
    );
  }

  // 用户菜单：绑定/解绑 OIDC 仅在 OIDC 已启用时出现，退出登录沿用原有确认逻辑。
  function userMenu(compact: boolean) {
    const showOIDC = !!oidcStatus?.enabled;
    return (
      <div
        role="menu"
        aria-label={t("账号操作")}
        className={cx(
          "absolute bottom-full z-20 mb-2 w-52 rounded-xl border border-gray-200/70 bg-white/95 p-1 shadow-xl backdrop-blur-xl dark:border-white/10 dark:bg-[#1c1c22]/95",
          compact ? "left-0" : "right-0",
        )}
      >
        {showOIDC && (
          <>
            {oidcStatus?.bound ? (
              <button
                type="button"
                role="menuitem"
                disabled={oidcBusy}
                onClick={() => { void onUnbindOIDC(); }}
                className={USER_MENU_ITEM_CLASS}
              >
                <OIDCIcon className="h-4 w-4 shrink-0" />
                <span>{oidcBusy ? t("正在解绑...") : t("解绑 OIDC 用户")}</span>
              </button>
            ) : (
              <button
                type="button"
                role="menuitem"
                onClick={openBindDialog}
                className={USER_MENU_ITEM_CLASS}
              >
                <OIDCIcon className="h-4 w-4 shrink-0" />
                <span>{t("绑定 OIDC 用户")}</span>
              </button>
            )}
            <div role="separator" className="my-1 h-px bg-gray-100 dark:bg-white/10" />
          </>
        )}
        <button
          type="button"
          role="menuitem"
          onClick={() => { setUserMenuOpen(false); void onLogout(); }}
          className={USER_MENU_LOGOUT_CLASS}
        >
          <SignOutRegular className="h-[18px] w-[18px] shrink-0" />
          <span>{t("退出登录")}</span>
        </button>
      </div>
    );
  }

  // 左下角用户卡片：齿轮按钮唤出菜单；collapse 时只渲染紧凑齿轮入口。
  function userCard(compact = false) {
    return (
      <div
        ref={userMenuRef}
        className={cx(
          "relative",
          compact
            ? "flex justify-center"
            : "ui-panel-muted flex items-center gap-3 p-3",
        )}
      >
        <button
          type="button"
          onClick={() => setUserMenuOpen((value) => !value)}
          aria-label={t("账号操作")}
          aria-haspopup="menu"
          aria-expanded={userMenuOpen}
          className={cx(
            "flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-indigo-50 text-indigo-600 transition-colors hover:bg-indigo-100 dark:bg-indigo-500/10 dark:text-indigo-300 dark:hover:bg-indigo-500/20",
            userMenuOpen && "ring-2 ring-indigo-500/40",
          )}
        >
          <SettingsRegular className="h-[18px] w-[18px]" />
        </button>
        {!compact && (
          <div className="min-w-0 flex-1">
            <div className="truncate text-sm font-bold">{user?.username || "Admin"}</div>
            <div className="truncate text-xs text-gray-400">{user?.role || "Administrator"}</div>
          </div>
        )}
        {userMenuOpen && userMenu(compact)}
      </div>
    );
  }

  return (
    <div className="flex h-full">
      {!isMobile && (
        <aside
          className={cx(
            "ui-glass sidebar-shell relative h-full transition-[width] duration-200",
            collapsed ? "w-[52px]" : "w-[232px]",
          )}
        >
          <div className={cx("flex h-14 items-center px-4", collapsed && "justify-center px-0")}>
            <BrandLogo className="sidebar-brand-logo" />
            {!collapsed && (
              <div className="ml-3">
                <div className="sidebar-brand-title">vocat</div>
                <div className="text-[10px] font-medium leading-tight tracking-wide text-gray-400 dark:text-gray-500">{t("高通模块测试工具")}</div>
              </div>
            )}
          </div>
          {menuList(collapsed)}
          {/* 折叠态仍渲染紧凑齿轮入口，保证用户菜单可用。 */}
          <div className={cx("absolute bottom-4 w-full", collapsed ? "px-1.5" : "px-3")}>
            {userCard(collapsed)}
          </div>
        </aside>
      )}

      <Drawer open={isMobile && mobileOpen} onClose={() => setMobileOpen(false)} className="mobile-drawer">
        <div className="sidebar-shell relative h-full bg-white/95 backdrop-blur-md dark:bg-[#141418]/95">
          <div className="flex h-16 items-center px-4">
            <BrandLogo className="sidebar-brand-logo" />
            <div className="ml-3">
              <div className="sidebar-brand-title">vocat</div>
              <div className="text-[10px] font-medium leading-tight tracking-wide text-gray-400 dark:text-gray-500">{t("高通模块测试工具")}</div>
            </div>
          </div>
          {menuList(false)}
          <div className="absolute bottom-4 w-full px-3">{userCard()}</div>
        </div>
      </Drawer>

      <div className="flex h-full min-w-0 flex-1 flex-col">
        <header className="ui-glass sticky top-0 z-10 flex h-14 items-center justify-between border-b border-gray-100 px-4 dark:border-white/5 sm:px-5">
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={toggle}
              aria-label={collapsed ? t("展开侧栏") : t("收起侧栏")}
              className="rounded-lg px-2 py-1.5 text-gray-500 transition-colors hover:bg-black/5 hover:text-gray-700 dark:text-gray-400 dark:hover:bg-white/10 dark:hover:text-gray-200"
            >
              {!isMobile && !collapsed ? (
                <PanelLeftContractRegular className="h-5 w-5" />
              ) : (
                <PanelLeftExpandRegular className="h-5 w-5" />
              )}
            </button>
          </div>
          <div className="flex items-center gap-3">
            <VersionBadge />
            <LanguageSwitch />
            <SwitchDark isDark={isDark} onToggle={onToggleTheme} />
          </div>
        </header>
        <main className="flex-1 overflow-auto bg-gray-50/50 p-4 dark:bg-transparent sm:p-6">
          <div className="main-inner mx-auto w-full">
            <ErrorBoundary title={t("页面渲染失败")}>
              <Outlet />
            </ErrorBoundary>
          </div>
        </main>
      </div>
      <IncomingCallWatcher />
      <OIDCBindDialog
        open={bindDialogOpen}
        onClose={closeBindDialog}
        onBound={onBoundOIDC}
      />
    </div>
  );
}
