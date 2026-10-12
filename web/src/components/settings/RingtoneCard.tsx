import { useCallback, useEffect, useRef, useState } from "react";
import { Speaker0Regular } from "@fluentui/react-icons";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { invalidateCustomRingtone, previewRingtone } from "../../lib/ringtone";
import { Button, Select, message } from "../ui";
import { CardDecor, CardIcon, CardTitle } from "./Cards";

const MAX_RINGTONE_BYTES = 8 * 1024 * 1024;

// 来电铃声设置：预设合成音或自行上传的音频文件；选择保存在服务端偏好中，
// 下一通来电即生效。
export function RingtoneCard() {
  const { t } = useI18n();
  const [choice, setChoice] = useState("classic");
  const [customName, setCustomName] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const prefs = await api<{ ringtone?: string }>("/settings/preferences");
      if (prefs.ringtone) setChoice(prefs.ringtone);
    } catch (error) {
      message.error(apiMessage(error) || t("铃声设置加载失败"));
    }
    try {
      // HEAD 探测是否已上传自定义铃声；文件名经 URL 编码在响应头返回。
      const response = await api<Response>("/settings/ringtone/custom", { method: "HEAD", raw: true });
      if (response.ok) {
        const encoded = response.headers.get("x-ringtone-name") || "";
        setCustomName(encoded ? decodeURIComponent(encoded) : t("自定义铃声"));
      } else {
        setCustomName("");
      }
    } catch {
      setCustomName("");
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const select = useCallback(
    async (value: string) => {
      setSaving(true);
      try {
        await api("/settings/preferences", { method: "PUT", body: { ringtone: value } });
        setChoice(value);
        message.success(t("铃声设置已保存，下一通来电生效"));
      } catch (error) {
        message.error(apiMessage(error) || t("铃声设置保存失败"));
      } finally {
        setSaving(false);
      }
    },
    [t],
  );

  const upload = useCallback(
    async (file: File) => {
      if (!file.type.startsWith("audio/")) {
        message.error(t("仅支持音频文件（mp3/ogg/wav/m4a/aac/webm）"));
        return;
      }
      if (file.size > MAX_RINGTONE_BYTES) {
        message.error(t("铃声文件不能超过 8MB"));
        return;
      }
      setUploading(true);
      try {
        const form = new FormData();
        form.append("file", file);
        const meta = await api<{ name?: string }>("/settings/ringtone/custom", { method: "PUT", body: form });
        invalidateCustomRingtone();
        setCustomName(meta.name || file.name || t("自定义铃声"));
        message.success(t("铃声上传成功"));
        // 上传成功后自动选中自定义铃声。
        await api("/settings/preferences", { method: "PUT", body: { ringtone: "custom" } });
        setChoice("custom");
      } catch (error) {
        message.error(apiMessage(error) || t("铃声上传失败"));
      } finally {
        setUploading(false);
      }
    },
    [t],
  );

  const removeCustom = useCallback(async () => {
    setDeleting(true);
    try {
      await api("/settings/ringtone/custom", { method: "DELETE" });
      invalidateCustomRingtone();
      setCustomName("");
      if (choice === "custom") setChoice("classic");
      message.success(t("自定义铃声已删除"));
    } catch (error) {
      message.error(apiMessage(error) || t("自定义铃声删除失败"));
    } finally {
      setDeleting(false);
    }
  }, [choice, t]);

  const options = [
    { value: "classic", label: t("经典双音") },
    { value: "single", label: t("单音") },
    { value: "chime", label: t("上升琶音") },
    { value: "beep", label: t("急促蜂鸣") },
    { value: "custom", label: customName ? `${t("自定义")}（${customName}）` : t("自定义（未上传）") },
  ];
  const busy = loading || saving || uploading || deleting;

  return (
    <div className="ui-card group relative overflow-hidden p-8">
      <CardDecor />
      <div className="relative z-10 mb-6 flex items-center gap-3">
        <CardIcon>
          <Speaker0Regular className="text-[24px]" />
        </CardIcon>
        <CardTitle
          title={t("来电铃声")}
          subtitle={t("来电时播放的提示音：可选预设合成音或自行上传的音频文件")}
        />
      </div>
      <div className="relative z-10 space-y-4">
        <Select
          value={choice}
          onChange={(value) => void select(value)}
          disabled={busy}
          options={options}
        />
        <div className="flex flex-wrap gap-2">
          <Button disabled={busy || (choice === "custom" && !customName)} onClick={() => previewRingtone(choice)}>
            {t("试听")}
          </Button>
          <Button disabled={busy} loading={uploading} onClick={() => fileInputRef.current?.click()}>
            {t("上传铃声")}
          </Button>
          {customName ? (
            <Button disabled={busy} loading={deleting} onClick={() => void removeCustom()}>
              {t("删除自定义铃声")}
            </Button>
          ) : null}
        </div>
        <input
          ref={fileInputRef}
          type="file"
          accept="audio/*"
          className="hidden"
          onChange={(event) => {
            const file = event.target.files?.[0];
            event.target.value = "";
            if (file) void upload(file);
          }}
        />
        <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">
          {t("支持 mp3/ogg/wav/m4a/aac/webm 音频，不超过 8MB；试听播放约 4 秒自动停止。")}
        </p>
      </div>
    </div>
  );
}
