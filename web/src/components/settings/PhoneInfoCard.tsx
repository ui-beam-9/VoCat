import { useCallback, useEffect, useState } from "react";
import { GlobeRegular } from "@fluentui/react-icons";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, Input, message } from "../ui";
import { CardDecor, CardIcon, CardTitle } from "./Cards";

interface PhoneInfoSettings {
  configured: boolean;
}

// 归属地 API Key：仅保存在服务器数据库中，保存后立即生效，无需改文件或重启。
export function PhoneInfoCard() {
  const { t } = useI18n();
  const [settings, setSettings] = useState<PhoneInfoSettings | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [apiKey, setApiKey] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setSettings(await api<PhoneInfoSettings>("/settings/phone-info"));
    } catch (error) {
      message.error(apiMessage(error) || t("归属地设置加载失败"));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const save = useCallback(
    async (value: string) => {
      setSaving(true);
      try {
        const next = await api<PhoneInfoSettings>("/settings/phone-info", {
          method: "PUT",
          body: { apiKey: value },
        });
        setSettings(next);
        setApiKey("");
        message.success(t("设置已保存并立即生效"));
      } catch (error) {
        message.error(apiMessage(error) || t("归属地设置保存失败"));
      } finally {
        setSaving(false);
      }
    },
    [t],
  );

  const configured = settings?.configured === true;

  return (
    <div className="ui-card group relative overflow-hidden p-8">
      <CardDecor />
      <div className="relative z-10 mb-6 flex items-center gap-3">
        <CardIcon>
          <GlobeRegular className="text-[24px]" />
        </CardIcon>
        <CardTitle
          title={t("号码归属地")}
          subtitle={t("通话中显示号码归属地的查询服务（uapis.cn）")}
        />
      </div>
      <div className="relative z-10 space-y-4">
        <div className="text-xs text-gray-500 dark:text-gray-400">
          {loading
            ? t("加载中...")
            : configured
              ? t("已配置 API Key")
              : t("未配置 API Key，将使用接口的访客额度")}
        </div>
        <Input
          type="password"
          value={apiKey}
          autoComplete="off"
          disabled={loading || saving}
          onChange={(event) => setApiKey(event.target.value)}
          placeholder={configured ? t("输入新的 API Key 以替换") : t("请输入 API Key")}
        />
        <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">
          {t("API Key 仅保存在本机数据库中，保存后立即生效，无需重启服务或修改容器文件。")}
        </p>
        <div className="flex gap-2">
          <Button
            variant="primary"
            className="flex-1 !border-0"
            loading={saving}
            disabled={loading || saving || apiKey.trim() === ""}
            onClick={() => void save(apiKey.trim())}
          >
            {t("保存 API Key")}
          </Button>
          <Button disabled={loading || saving || !configured} onClick={() => void save("")}>
            {t("清除")}
          </Button>
        </div>
      </div>
    </div>
  );
}
