import { DeleteRegular, EditRegular } from "@fluentui/react-icons";
import { cx } from "../../lib/utils";
import { useI18n } from "../../lib/i18n";
import type { Contact } from "../../lib/contacts";

export interface CallContactsListProps {
  contacts: Contact[];
  loading: boolean;
  locationByNumber?: Map<string, string>;
  onDial: (number: string) => void;
  onEdit: (contact: Contact) => void;
  onDelete: (contact: Contact) => void;
  deletingId: number | null;
}

// 本地通讯录（独立于短信）列表：一次最多三条，超出的滚动；支持编辑与删除。
const VISIBLE_ROWS_HEIGHT = 174;

export function CallContactsList({
  contacts,
  loading,
  locationByNumber,
  onDial,
  onEdit,
  onDelete,
  deletingId,
}: CallContactsListProps) {
  const { t } = useI18n();

  if (loading && contacts.length === 0) {
    return <div className="py-6 text-center text-sm text-gray-400">{t("加载中...")}</div>;
  }
  if (contacts.length === 0) {
    return <div className="py-6 text-center text-sm text-gray-400">{t("暂无联系人")}</div>;
  }

  return (
    <div className="space-y-1 overflow-y-auto pr-1" style={{ maxHeight: VISIBLE_ROWS_HEIGHT }}>
      {contacts.map((contact) => (
        <div
          key={contact.id}
          className="flex items-center justify-between gap-3 rounded-lg px-3 py-2 transition-colors hover:bg-black/5 dark:hover:bg-white/5"
        >
          <button type="button" onClick={() => onDial(contact.number)} className="min-w-0 flex-1 text-left">
            <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">{contact.name}</span>
            <span className="block truncate font-mono text-[10px] text-gray-400">
              {contact.number}
              {locationByNumber?.get(contact.number) ? `  · ${locationByNumber.get(contact.number)}` : ""}
            </span>
          </button>
          <div className="flex shrink-0 items-center gap-2 text-xs">
            <button type="button" className="text-[#0ea5e9]" onClick={() => onDial(contact.number)}>
              {t("拨打")}
            </button>
            <button
              type="button"
              aria-label={t("编辑")}
              title={t("编辑")}
              className="text-gray-500 transition-colors hover:text-gray-800 dark:text-gray-400 dark:hover:text-gray-200"
              onClick={() => onEdit(contact)}
            >
              <EditRegular className="h-4 w-4" />
            </button>
            <button
              type="button"
              aria-label={t("删除")}
              title={t("删除")}
              disabled={deletingId === contact.id}
              className={cx(
                "text-gray-400 transition-colors hover:text-red-500 disabled:opacity-50",
                deletingId === contact.id && "animate-pulse",
              )}
              onClick={() => onDelete(contact)}
            >
              <DeleteRegular className="h-4 w-4" />
            </button>
          </div>
        </div>
      ))}
    </div>
  );
}
