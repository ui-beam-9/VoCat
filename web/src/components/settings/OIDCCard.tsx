import { useCallback, useEffect, useState } from "react";
import {
  AddRegular,
  ArrowSyncRegular,
  CheckmarkRegular,
  DeleteRegular,
} from "@fluentui/react-icons";
import {
  apiMessage,
  deleteOIDCBinding,
  discoverOIDC,
  getOIDCSettings,
  updateOIDCSettings,
} from "../../api";
import type { OIDCBinding, OIDCDiscovery, OIDCSettings } from "../../types";
import { useI18n } from "../../lib/i18n";
import { Button, Input, Switch, confirmDialog, message } from "../ui";
import { OIDCIcon } from "../ui/OIDCIcon";
import { CardDecor, CardIcon, CardTitle } from "./Cards";
import { EmptyLine, Field, FieldRow, PasswordInput } from "./controls";

// 后端返回的脱敏占位符：回传时后端会保留数据库里的旧密钥。
const SECRET_MASK = "********";

// 表单态：scope 与回调地址在提交时再做切分/去空白，输入过程中保留用户原文。
interface OIDCForm {
  enabled: boolean;
  wellKnownUrl: string;
  clientId: string;
  clientSecret: string;
  scopes: string;
  callbackUrls: string[];
  loginButtonText: string;
}

const EMPTY_FORM: OIDCForm = {
  enabled: false,
  wellKnownUrl: "",
  clientId: "",
  clientSecret: "",
  scopes: "",
  callbackUrls: [],
  loginButtonText: "",
};

// 把接口返回的设置对象转成表单态。
function settingsToForm(data: OIDCSettings): OIDCForm {
  const scopes = data.scopes?.length ? data.scopes : (data.defaultScopes ?? []);
  return {
    enabled: !!data.enabled,
    wellKnownUrl: data.wellKnownUrl ?? "",
    clientId: data.clientId ?? "",
    clientSecret: data.clientSecret ?? "",
    scopes: scopes.join(" "),
    callbackUrls: data.callbackUrls ?? [],
    loginButtonText: data.loginButtonText ?? "",
  };
}

// scope 支持空格或逗号分隔。
function parseScopes(value: string): string[] {
  return value
    .split(/[\s,]+/)
    .map((item) => item.trim())
    .filter(Boolean);
}

// 绑定时间格式化：解析失败时原样展示后端字符串。
function formatBoundAt(value: string): string {
  if (!value) return "";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

export function OIDCCard() {
  const { t } = useI18n();
  const [settings, setSettings] = useState<OIDCSettings | null>(null);
  const [form, setForm] = useState<OIDCForm>(EMPTY_FORM);
  const [discovery, setDiscovery] = useState<OIDCDiscovery | null>(null);
  const [binding, setBinding] = useState<OIDCBinding | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [discovering, setDiscovering] = useState(false);
  const [unbinding, setUnbinding] = useState(false);

  // 接口返回的统一回填入口。
  const applySettings = useCallback((data: OIDCSettings) => {
    setSettings(data);
    setForm(settingsToForm(data));
    setDiscovery(data.discovery ?? null);
    setBinding(data.binding ?? null);
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      applySettings(await getOIDCSettings());
    } catch (error) {
      message.error(apiMessage(error) || t("OIDC 配置加载失败"));
    } finally {
      setLoading(false);
    }
  }, [applySettings, t]);

  useEffect(() => {
    void load();
  }, [load]);

  const patch = useCallback((next: Partial<OIDCForm>) => {
    setForm((prev) => ({ ...prev, ...next }));
  }, []);

  // 当前访问地址对应的回调地址：origin + 后端约定的回调路径。
  const callbackBase = window.location.origin + (settings?.callbackPath || "/api/auth/oidc/callback");

  const addCallbackURL = useCallback(() => {
    setForm((prev) => ({ ...prev, callbackUrls: [...prev.callbackUrls, ""] }));
  }, []);

  const updateCallbackURL = useCallback((index: number, value: string) => {
    setForm((prev) => ({
      ...prev,
      callbackUrls: prev.callbackUrls.map((item, i) => (i === index ? value : item)),
    }));
  }, []);

  const removeCallbackURL = useCallback((index: number) => {
    setForm((prev) => ({
      ...prev,
      callbackUrls: prev.callbackUrls.filter((_, i) => i !== index),
    }));
  }, []);

  // 用当前访问地址生成一条回调地址；已存在时不重复追加。
  const generateCallbackURL = useCallback(() => {
    if (form.callbackUrls.includes(callbackBase)) {
      message.info(t("当前访问地址对应的回调地址已存在"));
      return;
    }
    setForm((prev) => ({ ...prev, callbackUrls: [...prev.callbackUrls, callbackBase] }));
  }, [callbackBase, form.callbackUrls, t]);

  // 自动获取：只做 Well-known 预检并展示端点，不落库。
  const onDiscover = useCallback(async () => {
    const url = form.wellKnownUrl.trim();
    if (!url) {
      message.error(t("请先填写 Well-known 配置地址"));
      return;
    }
    setDiscovering(true);
    try {
      const data = await discoverOIDC(url);
      setDiscovery(data);
      message.success(t("端点发现成功"));
    } catch (error) {
      message.error(apiMessage(error) || t("端点发现失败"));
    } finally {
      setDiscovering(false);
    }
  }, [form.wellKnownUrl, t]);

  // 保存：脱敏或空的 clientSecret 原样回传，后端会保留旧值。
  const onSave = useCallback(async () => {
    setSaving(true);
    try {
      const data = await updateOIDCSettings({
        enabled: form.enabled,
        wellKnownUrl: form.wellKnownUrl.trim(),
        clientId: form.clientId.trim(),
        clientSecret: form.clientSecret,
        scopes: parseScopes(form.scopes),
        callbackUrls: form.callbackUrls.map((url) => url.trim()).filter(Boolean),
        loginButtonText: form.loginButtonText.trim(),
      });
      applySettings(data);
      message.success(t("OIDC 配置已保存"));
    } catch (error) {
      message.error(apiMessage(error) || t("OIDC 配置保存失败"));
    } finally {
      setSaving(false);
    }
  }, [form, applySettings, t]);

  // 解绑：二次确认后删除绑定关系并刷新卡片。
  const onUnbind = useCallback(async () => {
    const confirmed = await confirmDialog(
      t("解绑后将无法使用该 OIDC 身份登录，密码登录不受影响。确定要解绑吗？"),
      t("解绑 OIDC 用户"),
      {
        type: "warning",
        confirmVariant: "danger",
        confirmText: t("解绑"),
        cancelText: t("取消"),
      },
    );
    if (!confirmed) return;
    setUnbinding(true);
    try {
      await deleteOIDCBinding();
      message.success(t("OIDC 绑定已解除"));
      applySettings(await getOIDCSettings());
    } catch (error) {
      message.error(apiMessage(error) || t("解绑 OIDC 用户失败"));
    } finally {
      setUnbinding(false);
    }
  }, [applySettings, t]);

  const busy = loading || saving;
  const secretPlaceholder =
    form.clientSecret === SECRET_MASK ? t("已保存密钥，留空表示不修改") : t("请输入 Client Secret");

  return (
    <div className="ui-card group relative overflow-hidden p-8 lg:col-span-2">
      <CardDecor />
      <div className="relative z-10 mb-6 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-3">
          <CardIcon>
            <OIDCIcon className="h-6 w-6" />
          </CardIcon>
          <CardTitle
            title={t("OIDC 单点登录")}
            subtitle={t("接入统一身份认证，登录页可显示 OIDC 登录入口")}
          />
        </div>
        <div className="flex items-center gap-2">
          <span className="text-xs font-bold uppercase tracking-wider text-gray-500">{t("启用")}</span>
          <Switch
            checked={form.enabled}
            disabled={busy}
            onChange={(enabled) => patch({ enabled })}
            ariaLabel={t("启用 OIDC 登录")}
          />
        </div>
      </div>

      {loading ? (
        <div className="relative z-10 space-y-2 text-sm text-gray-500 dark:text-gray-400">
          <div>{t("正在加载 OIDC 配置…")}</div>
        </div>
      ) : (
        <div className="relative z-10 space-y-5">
          <Field
            label={t("Well-known 配置地址")}
            hint={t("填写完整地址，例如 https://idp.example.com/.well-known/openid-configuration；也可直接填写 issuer 地址。")}
          >
            <div className="flex flex-col gap-2 sm:flex-row">
              <Input
                value={form.wellKnownUrl}
                disabled={busy || discovering}
                placeholder="https://idp.example.com/.well-known/openid-configuration"
                onChange={(event) => patch({ wellKnownUrl: event.target.value })}
                className="flex-1"
              />
              <Button
                loading={discovering}
                disabled={busy}
                icon={<ArrowSyncRegular />}
                onClick={() => void onDiscover()}
              >
                {t("自动获取")}
              </Button>
            </div>
          </Field>

          {discovery ? (
            <div className="space-y-2 rounded-xl bg-gray-50 p-4 text-sm dark:bg-white/5">
              <div className="text-xs font-bold uppercase tracking-wider text-gray-500">{t("已发现的端点")}</div>
              <FieldRow label="Issuer" value={discovery.issuer} monospace copyable />
              <FieldRow label={t("授权端点")} value={discovery.authorizationEndpoint} monospace copyable />
              <FieldRow label={t("Token 端点")} value={discovery.tokenEndpoint} monospace copyable />
              <FieldRow label={t("UserInfo 端点")} value={discovery.userinfoEndpoint} monospace copyable />
              <FieldRow label={t("登出端点")} value={discovery.endSessionEndpoint} monospace copyable />
              <FieldRow label={t("支持的 Scope")} value={discovery.scopesSupported?.join(" ")} monospace />
              {discovery.error ? (
                <div className="break-words text-xs text-amber-600 dark:text-amber-400">
                  {t("最近一次端点发现失败：")} {discovery.error}
                </div>
              ) : null}
            </div>
          ) : null}

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label="Client ID">
              <Input
                value={form.clientId}
                disabled={busy}
                autoComplete="off"
                onChange={(event) => patch({ clientId: event.target.value })}
              />
            </Field>
            <Field label="Client Secret" hint={t("已保存时显示为脱敏值；留空或保留脱敏值提交都不会清除旧密钥。")}>
              <PasswordInput
                value={form.clientSecret}
                disabled={busy}
                placeholder={secretPlaceholder}
                autoComplete="new-password"
                onChange={(clientSecret) => patch({ clientSecret })}
              />
            </Field>
          </div>

          <Field label="Scope" hint={t("多个 scope 用空格或逗号分隔，留空时后端使用默认值。")}>
            <Input
              value={form.scopes}
              disabled={busy}
              placeholder={(settings?.defaultScopes ?? []).join(" ")}
              onChange={(event) => patch({ scopes: event.target.value })}
            />
          </Field>

          <div className="space-y-2">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <label className="text-xs font-bold uppercase tracking-wider text-gray-500">
                {t("回调地址（Callback URL）")}
              </label>
              <div className="flex flex-wrap gap-2">
                <Button
                  size="small"
                  variant="primary"
                  plain
                  icon={<AddRegular />}
                  disabled={busy}
                  onClick={addCallbackURL}
                >
                  {t("添加回调地址")}
                </Button>
                <Button size="small" plain disabled={busy} onClick={generateCallbackURL}>
                  {t("根据当前访问地址生成")}
                </Button>
              </div>
            </div>
            {form.callbackUrls.length === 0 ? (
              <EmptyLine>{t("尚未配置回调地址，OIDC 授权回调将无法完成")}</EmptyLine>
            ) : null}
            {form.callbackUrls.map((url, index) => (
              <div key={index} className="flex items-center gap-2">
                <Input
                  value={url}
                  disabled={busy}
                  placeholder="https://vocat.example.com/api/auth/oidc/callback"
                  onChange={(event) => updateCallbackURL(index, event.target.value)}
                  className="flex-1"
                />
                <Button
                  variant="danger"
                  plain
                  disabled={busy}
                  aria-label={t("删除回调地址")}
                  icon={<DeleteRegular />}
                  onClick={() => removeCallbackURL(index)}
                />
              </div>
            ))}
            <p className="text-xs leading-5 text-amber-600 dark:text-amber-400">
              {t("需在 OIDC 服务端（IDP）登记上述所有回调地址，只登记其中一个会导致其他访问方式授权失败。")}
            </p>
          </div>

          <Field label={t("登录页按钮文案")} hint={t("留空时使用默认文案。")}>
            <Input
              value={form.loginButtonText}
              disabled={busy}
              placeholder={settings?.defaultLoginButtonText}
              onChange={(event) => patch({ loginButtonText: event.target.value })}
            />
          </Field>

          {settings && !settings.configured ? (
            <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">
              {t("配置尚不完整：需填写 Well-known 地址与 Client ID，并成功发现端点后才能启用。")}
            </p>
          ) : null}

          <div className="pt-2">
            <Button
              variant="primary"
              className="w-full !border-0"
              loading={saving}
              disabled={loading || discovering}
              icon={<CheckmarkRegular />}
              onClick={() => void onSave()}
            >
              {t("保存 OIDC 配置")}
            </Button>
          </div>

          <div className="space-y-3 border-t border-gray-200 pt-4 text-sm dark:border-white/10">
            <div className="font-bold text-gray-800 dark:text-gray-100">{t("当前绑定信息")}</div>
            {binding ? (
              <>
                <div className="space-y-2 rounded-xl bg-gray-50 p-4 dark:bg-white/5">
                  <FieldRow label="Issuer" value={binding.issuer} monospace copyable />
                  <FieldRow label="Subject" value={binding.subject} monospace copyable />
                  <FieldRow label={t("邮箱")} value={binding.email} monospace copyable />
                  <FieldRow label={t("名称")} value={binding.name} />
                  <FieldRow label={t("绑定时间")} value={formatBoundAt(binding.boundAt)} monospace />
                </div>
                <Button
                  variant="danger"
                  plain
                  loading={unbinding}
                  disabled={busy || discovering}
                  onClick={() => void onUnbind()}
                >
                  {t("解绑 OIDC 用户")}
                </Button>
              </>
            ) : (
              <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">
                {t("尚未绑定 OIDC 用户；可在左下角用户菜单发起绑定，绑定后才能使用 OIDC 登录。")}
              </p>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
