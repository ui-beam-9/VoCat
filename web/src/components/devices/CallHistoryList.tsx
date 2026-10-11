import { DeleteRegular } from "@fluentui/react-icons";
import { cx } from "../../lib/utils";
import { useI18n } from "../../lib/i18n";
import type { CallDirection, CallHistoryEntry, CallOutcome } from "../../lib/callHistory";

export interface CallHistoryListProps {
  entries: CallHistoryEntry[];
  contactNameByNumber: Map<string, string>;
  locationByNumber?: Map<string, string>;
  outcomeLabel: (outcome: CallOutcome, direction: CallDirection) => string;
  onDial: (number: string) => void;
  onSave: (entry: CallHistoryEntry) => void;
  onDelete: (entry: CallHistoryEntry) => void;
  deletingId: number | null;
}

// 一次最多显示三条通话记录，超出的滚动查看。独立组件，避免页面被无限撑长。
const VISIBLE_ROWS_HEIGHT = 174;

function formatDuration(seconds: number): string {
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return `${minutes}:${String(rest).padStart(2, "0")}`;
}

function formatTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

export function CallHistoryList({
  entries,
  contactNameByNumber,
  locationByNumber,
  outcomeLabel,
  onDial,
  onSave,
  onDelete,
  deletingId,
}: CallHistoryListProps) {
  const { t } = useI18n();

  if (entries.length === 0) {
    return <div className="py-6 text-center text-sm text-gray-400">{t("暂无通话记录")}</div>;
  }

  return (
    <div className="space-y-1 overflow-y-auto pr-1" style={{ maxHeight: VISIBLE_ROWS_HEIGHT }}>
      {entries.map((entry) => {
        const name = contactNameByNumber.get(entry.number);
        return (
          <div
            key={entry.id}
            className="flex items-center justify-between gap-3 rounded-lg px-3 py-2 transition-colors hover:bg-black/5 dark:hover:bg-white/5"
          >
            <button type="button" onClick={() => onDial(entry.number)} className="min-w-0 flex-1 text-left">
              {name ? (
                <>
                  <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">{name}</span>
                  <span className="block truncate font-mono text-[10px] text-gray-400">{entry.number}</span>
                </>
              ) : (
                <span className="block truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                  {entry.number || t("未知号码")}
                </span>
              )}
              <span className="block text-xs text-gray-500 dark:text-gray-400">
                {entry.direction === "incoming" ? t("来电") : t("呼出")} ·{" "}
                {outcomeLabel(entry.outcome, entry.direction)} · {formatTime(entry.startedAt)}
                {entry.durationSeconds > 0 ? ` · ${formatDuration(entry.durationSeconds)}` : ""}
                {locationByNumber?.get(entry.number) ? ` · ${locationByNumber.get(entry.number)}` : ""}
              </span>
            </button>
            <div className="flex shrink-0 items-center gap-2 text-xs">
              <button type="button" className="text-[#0ea5e9]" onClick={() => onDial(entry.number)}>
                {t("拨打")}
              </button>
              <button
                type="button"
                className="text-gray-500 transition-colors hover:text-gray-800 dark:text-gray-400 dark:hover:text-gray-200"
                onClick={() => onSave(entry)}
              >
                {t("保存")}
              </button>
              <button
                type="button"
                aria-label={t("删除")}
                title={t("删除")}
                disabled={deletingId === entry.id}
                className={cx(
                  "text-gray-400 transition-colors hover:text-red-500 disabled:opacity-50",
                  deletingId === entry.id && "animate-pulse",
                )}
                onClick={() => onDelete(entry)}
              >
                <DeleteRegular className="h-4 w-4" />
              </button>
            </div>
          </div>
        );
      })}
    </div>
  );
}
