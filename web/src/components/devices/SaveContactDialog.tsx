import { useEffect, useState } from "react";
import { Button, Input, Modal } from "../ui";
import { useI18n } from "../../lib/i18n";
import { validateCallNumber } from "./callApi";

export interface SaveContactDialogProps {
  open: boolean;
  initialName?: string;
  initialNumber: string;
  // 标题默认为「保存为联系人」，编辑场景传入「编辑联系人」。
  title?: string;
  onClose: () => void;
  onSave: (name: string, number: string) => void;
}

// 保存/编辑联系人：输入姓名并确认号码。
export function SaveContactDialog({
  open,
  initialName = "",
  initialNumber,
  title,
  onClose,
  onSave,
}: SaveContactDialogProps) {
  const { t } = useI18n();
  const [name, setName] = useState("");
  const [number, setNumber] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    if (open) {
      setName(initialName);
      setNumber(initialNumber || "");
      setError("");
    }
  }, [open, initialName, initialNumber]);

  function handleSave() {
    const trimmedName = name.trim();
    const trimmedNumber = number.trim();
    if (!trimmedName) {
      setError(t("姓名不能为空"));
      return;
    }
    if (!validateCallNumber(trimmedNumber).valid) {
      setError(t("号码格式不正确：请输入 2-32 位数字，可含首位 +、*、#"));
      return;
    }
    onSave(trimmedName, trimmedNumber);
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title ?? t("保存为联系人")}
      footer={
        <>
          <Button onClick={onClose}>{t("取消")}</Button>
          <Button variant="primary" onClick={handleSave}>
            {t("保存")}
          </Button>
        </>
      }
    >
      <div className="mt-2 space-y-4">
        <div>
          <label className="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{t("姓名")}</label>
          <Input
            value={name}
            autoFocus
            onChange={(event) => {
              setName(event.target.value);
              setError("");
            }}
            onKeyDown={(event) => {
              if (event.key === "Enter") handleSave();
            }}
            placeholder={t("请输入姓名")}
          />
        </div>
        <div>
          <label className="mb-1.5 block text-sm font-medium text-gray-700 dark:text-gray-300">{t("号码")}</label>
          <Input
            value={number}
            onChange={(event) => {
              setNumber(event.target.value);
              setError("");
            }}
            onKeyDown={(event) => {
              if (event.key === "Enter") handleSave();
            }}
            placeholder={t("请输入号码")}
          />
        </div>
        {error ? <div className="text-xs text-red-500">{error}</div> : null}
      </div>
    </Modal>
  );
}
