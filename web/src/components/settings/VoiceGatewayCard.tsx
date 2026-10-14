import { useCallback, useEffect, useState } from "react";
import {
  AddRegular,
  AlertRegular,
  ArrowSyncRegular,
  CallRegular,
  DeleteRegular,
  SaveRegular,
} from "@fluentui/react-icons";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import type { DevicesResponse } from "../../types";
import { Button } from "../ui/Button";
import { Input } from "../ui/Input";
import { Select, type SelectOption } from "../ui/Select";
import { StatusDot } from "../ui/StatusDot";
import { Switch } from "../ui/Switch";
import { Tag } from "../ui/Tag";
import { message } from "../ui/message";
import { CardDecor, CardIcon, CardTitle } from "./Cards";
import { EmptyLine, Field, FieldRow, PasswordInput } from "./controls";

// 后端返回的密码永远是掩码字符串；提交时保留掩码或留空都不会覆盖已保存的密码。
const PASSWORD_MASK = "********";

interface VoiceGatewayUser {
  username: string;
  password: string;
  displayName: string;
  deviceId: string;
}

interface VoiceGatewayConfig {
  enabled: boolean;
  listen: string;
  transport: string;
  realm: string;
  externalIp: string;
  backend: string;
  rtpPortMin: number;
  rtpPortMax: number;
  users: VoiceGatewayUser[];
}

interface VoiceGatewayDeviceStatus {
  deviceId: string;
  audioDevice: string;
  backend: string;
  active: boolean;
  detail: string;
}

interface VoiceGatewayStatus {
  enabled: boolean;
  running: boolean;
  listen: string;
  realm: string;
  externalIp: string;
  userCount: number;
  registered: number;
  devices: VoiceGatewayDeviceStatus[];
  lastError: string;
  startedAt: string;
}

interface VoiceGatewayResponse {
  config: VoiceGatewayConfig;
  status: VoiceGatewayStatus;
}

// 表单里的端口字段用字符串承载，提交时再转数字，避免空输入被强制显示为 0。
interface VoiceGatewayForm {
  enabled: boolean;
  listen: string;
  transport: string;
  realm: string;
  externalIp: string;
  backend: string;
  rtpPortMin: string;
  rtpPortMax: string;
  users: VoiceGatewayUser[];
}

const TRANSPORT_OPTIONS: SelectOption[] = [
  { value: "udp", label: "UDP" },
  { value: "tcp", label: "TCP" },
];

const LABEL_CLASS = "text-xs font-bold text-gray-500 uppercase tracking-wider";

const EMPTY_STATUS: VoiceGatewayStatus = {
  enabled: false,
  running: false,
  listen: "",
  realm: "",
  externalIp: "",
  userCount: 0,
  registered: 0,
  devices: [],
  lastError: "",
  startedAt: "",
};

function configToForm(config: VoiceGatewayConfig): VoiceGatewayForm {
  return {
    enabled: !!config.enabled,
    listen: config.listen || "",
    transport: config.transport || "udp",
    realm: config.realm || "",
    externalIp: config.externalIp || "",
    backend: config.backend || "auto",
    rtpPortMin: String(config.rtpPortMin ?? 10000),
    rtpPortMax: String(config.rtpPortMax ?? 20000),
    users: Array.isArray(config.users) ? config.users : [],
  };
}

export function VoiceGatewayCard() {
  const { t } = useI18n();
  const [form, setForm] = useState<VoiceGatewayForm>(() => configToForm({
    enabled: false,
    listen: "0.0.0.0:5060",
    transport: "udp",
    realm: "vocat.local",
    externalIp: "",
    backend: "auto",
    rtpPortMin: 10000,
    rtpPortMax: 20000,
    users: [],
  }));
  const [status, setStatus] = useState<VoiceGatewayStatus>(EMPTY_STATUS);
  const [devices, setDevices] = useState<{ id: string; name: string }[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadFailed, setLoadFailed] = useState(false);
  const [saving, setSaving] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setLoadFailed(false);
    try {
      const [data, deviceData] = await Promise.all([
        api<VoiceGatewayResponse>("/settings/voice-gateway"),
        api<DevicesResponse>("/devices"),
      ]);
      if (data?.config) setForm(configToForm(data.config));
      setStatus(data?.status || EMPTY_STATUS);
      setDevices((deviceData?.devices || []).map((d) => ({ id: d.id, name: d.name })));
    } catch (error) {
      setLoadFailed(true);
      message.error(apiMessage(error) || t("语音网关配置加载失败"));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const patch = (next: Partial<VoiceGatewayForm>) => setForm((prev) => ({ ...prev, ...next }));

  const addUser = () =>
    setForm((prev) => ({
      ...prev,
      users: [...prev.users, { username: "", password: "", displayName: "", deviceId: "" }],
    }));

  const removeUser = (index: number) =>
    setForm((prev) => ({ ...prev, users: prev.users.filter((_, i) => i !== index) }));

  const updateUser = (index: number, next: Partial<VoiceGatewayUser>) =>
    setForm((prev) => ({
      ...prev,
      users: prev.users.map((user, i) => (i === index ? { ...user, ...next } : user)),
    }));

  const onSave = useCallback(async () => {
    setSaving(true);
    try {
      const payload = {
        enabled: !!form.enabled,
        listen: form.listen,
        transport: form.transport || "udp",
        realm: form.realm,
        externalIp: form.externalIp,
        backend: form.backend || "auto",
        rtpPortMin: Number(form.rtpPortMin) || 0,
        rtpPortMax: Number(form.rtpPortMax) || 0,
        users: form.users.map((user) => ({
          username: user.username,
          password: user.password,
          displayName: user.displayName,
          deviceId: user.deviceId,
        })),
      };
      const data = await api<VoiceGatewayResponse>("/settings/voice-gateway", {
        method: "PUT",
        body: payload,
      });
      // 保存成功后回填后端返回的配置（密码会重新变成掩码）与最新运行状态。
      if (data?.config) setForm(configToForm(data.config));
      setStatus(data?.status || EMPTY_STATUS);
      message.success(t("语音网关配置已保存"));
    } catch (error) {
      message.error(apiMessage(error) || t("语音网关配置保存失败"));
    } finally {
      setSaving(false);
    }
  }, [form, t]);

  const deviceOptions: SelectOption[] = devices.map((d) => ({
    value: d.id,
    label: d.name ? `${d.name} (${d.id})` : d.id,
  }));

  const backendOptions: SelectOption[] = [
    { value: "auto", label: t("自动") },
    { value: "ims", label: "IMS (VoWiFi)" },
    { value: "cs", label: t("电路域 (CS)") },
  ];

  return (
    <div className="ui-card group relative overflow-hidden p-8 lg:col-span-2">
      <CardDecor />
      <div className="relative z-10 mb-6 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-3">
          <CardIcon>
            <CallRegular className="text-[24px]" />
          </CardIcon>
          <CardTitle title={t("语音网关")} subtitle={t("手机软电话通过 4G 模组接打电话（VoWiFi 或电路域）")} />
        </div>
        <div className="flex items-center gap-3">
          <Switch
            checked={form.enabled}
            disabled={loading || loadFailed || saving}
            onChange={(enabled) => patch({ enabled })}
            ariaLabel={t("启用语音网关")}
          />
          <Button
            variant="primary"
            loading={saving}
            disabled={loading || loadFailed}
            onClick={onSave}
            className="!border-0"
            icon={<SaveRegular />}
          >
            {t("保存配置")}
          </Button>
        </div>
      </div>

      {loading ? (
        <div className="relative z-10 p-6 text-sm text-gray-500 dark:text-gray-400">
          {t("正在加载语音网关配置…")}
        </div>
      ) : loadFailed ? (
        <div className="relative z-10 space-y-3 p-6 text-sm text-gray-500 dark:text-gray-400">
          <div>{t("语音网关配置加载失败")}</div>
          <Button size="small" variant="primary" plain onClick={() => void load()} icon={<ArrowSyncRegular />}>
            {t("重试")}
          </Button>
        </div>
      ) : (
        <div className="relative z-10 space-y-6">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label={t("SIP 监听地址")}>
              <Input value={form.listen} onChange={(e) => patch({ listen: e.target.value })} placeholder="0.0.0.0:5060" className="font-mono" />
            </Field>
            <Field label={t("传输协议")}>
              <Select value={form.transport} onChange={(transport) => patch({ transport })} options={TRANSPORT_OPTIONS} />
            </Field>
            <Field label={t("桥接后端")} hint={t("自动模式优先使用 VoWiFi，不可用时回退电路域（需为设备配置声卡）")}>
              <Select value={form.backend} onChange={(backend) => patch({ backend })} options={backendOptions} />
            </Field>
            <Field label={t("认证域")}>
              <Input value={form.realm} onChange={(e) => patch({ realm: e.target.value })} placeholder="vocat.local" className="font-mono" />
            </Field>
            <Field label={t("对外 IP")} hint={t("对外 IP 留空表示自动探测")}>
              <Input value={form.externalIp} onChange={(e) => patch({ externalIp: e.target.value })} placeholder="1.2.3.4" className="font-mono" />
            </Field>
            <Field label={t("RTP 端口范围")} className="sm:col-span-2">
              <div className="flex items-center gap-2">
                <Input
                  type="number"
                  inputMode="numeric"
                  value={form.rtpPortMin}
                  onChange={(e) => patch({ rtpPortMin: e.target.value })}
                  placeholder={t("最小端口")}
                />
                <span className="shrink-0 text-gray-400">-</span>
                <Input
                  type="number"
                  inputMode="numeric"
                  value={form.rtpPortMax}
                  onChange={(e) => patch({ rtpPortMax: e.target.value })}
                  placeholder={t("最大端口")}
                />
              </div>
            </Field>
          </div>

          <div className="space-y-2">
            <div className="flex items-center justify-between">
              <label className={LABEL_CLASS}>{t("注册用户")}</label>
              <Button size="small" variant="primary" plain onClick={addUser} icon={<AddRegular />}>
                {t("添加用户")}
              </Button>
            </div>
            {form.users.length === 0 ? (
              <EmptyLine>{t("暂无用户，点击「添加用户」创建 Linphone 注册账号")}</EmptyLine>
            ) : null}
            {form.users.map((user, index) => (
              <div key={index} className="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-white/10">
                <div className="flex items-center justify-between">
                  <span className="text-sm font-bold text-gray-800 dark:text-gray-100">
                    {t("用户")} {index + 1}
                  </span>
                  <Button
                    variant="danger"
                    plain
                    size="small"
                    onClick={() => removeUser(index)}
                    aria-label={t("删除")}
                    icon={<DeleteRegular />}
                  />
                </div>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field label={t("用户名")}>
                    <Input value={user.username} onChange={(e) => updateUser(index, { username: e.target.value })} placeholder="linphone" className="font-mono" />
                  </Field>
                  <Field
                    label={
                      <span className="inline-flex items-center gap-2">
                        {t("密码")}
                        {user.password === PASSWORD_MASK ? <Tag type="success">{t("已设密码")}</Tag> : null}
                      </span>
                    }
                  >
                    <PasswordInput
                      value={user.password === PASSWORD_MASK ? "" : user.password}
                      onChange={(password) => updateUser(index, { password })}
                      placeholder={user.password === PASSWORD_MASK ? t("留空表示保持原密码") : t("请输入密码")}
                      autoComplete="new-password"
                    />
                  </Field>
                  <Field label={t("显示名称")}>
                    <Input value={user.displayName} onChange={(e) => updateUser(index, { displayName: e.target.value })} placeholder={t("选填")} />
                  </Field>
                  <Field label={t("设备")}>
                    <Select
                      value={user.deviceId}
                      onChange={(deviceId) => updateUser(index, { deviceId })}
                      options={deviceOptions}
                      placeholder={t("选择设备")}
                    />
                  </Field>
                </div>
              </div>
            ))}
          </div>

          <div className="space-y-3 rounded-lg bg-gray-50 p-4 dark:bg-white/5">
            <div className="flex items-center gap-2">
              <StatusDot tone={status.running ? "success" : "neutral"} />
              <span className="text-sm font-bold text-gray-800 dark:text-gray-100">
                {status.running ? t("运行中") : t("已停止")}
              </span>
            </div>
            <div className="rounded-lg bg-white p-3 dark:bg-white/5">
              <FieldRow label={t("监听地址")} value={status.listen} monospace />
              <FieldRow label={t("认证域")} value={status.realm} monospace />
              <FieldRow label={t("对外 IP")} value={status.externalIp} monospace />
              <FieldRow label={t("用户数")} value={status.userCount} />
              <FieldRow label={t("已注册")} value={status.registered} />
              <FieldRow label={t("启动时间")} value={status.startedAt} monospace />
            </div>

            {status.lastError ? (
              <div className="flex items-start gap-2 rounded-lg border border-red-200 bg-red-50 p-3 text-xs text-red-700 dark:border-red-500/20 dark:bg-red-500/10 dark:text-red-300">
                <AlertRegular className="mt-0.5 shrink-0" />
                <div className="min-w-0">
                  <div className="font-bold">{t("最近错误")}</div>
                  <div className="whitespace-pre-wrap break-words font-mono">{status.lastError}</div>
                </div>
              </div>
            ) : null}

            <div className="space-y-2">
              <div className={LABEL_CLASS}>{t("绑定设备状态")}</div>
              {status.devices.length === 0 ? (
                <EmptyLine>{t("暂无绑定设备")}</EmptyLine>
              ) : (
                status.devices.map((device) => (
                  <div
                    key={device.deviceId}
                    className="flex items-start justify-between gap-3 rounded-lg border border-gray-200 bg-white px-3 py-2 dark:border-white/10 dark:bg-white/5"
                  >
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="truncate font-mono text-sm text-gray-800 dark:text-gray-100">{device.deviceId}</span>
                        <Tag type={device.active ? "success" : "danger"}>{device.active ? t("活跃") : t("未激活")}</Tag>
                      </div>
                      <div className="text-xs text-gray-500 dark:text-gray-400">
                        {device.backend === "ims"
                          ? t("桥接后端：IMS (VoWiFi)，无需声卡")
                          : `${t("音频设备")}: ${device.audioDevice || t("未配置音频设备")}`}
                      </div>
                    </div>
                    {device.detail ? (
                      <span className="shrink-0 text-xs text-amber-600 dark:text-amber-400">{device.detail}</span>
                    ) : null}
                  </div>
                ))
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
