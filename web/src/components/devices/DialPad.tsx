import { cx } from "../../lib/utils";

// 电话号码键盘：1-9 / * / 0 / #，外加 +、退格与呼叫按钮。
// 通话中（mode="dtmf"）时隐藏 +/退格，按键直接作为 DTMF 发送，主按钮变为挂断。
const KEYS: Array<{ key: string; sub?: string }> = [
  { key: "1" },
  { key: "2", sub: "ABC" },
  { key: "3", sub: "DEF" },
  { key: "4", sub: "GHI" },
  { key: "5", sub: "JKL" },
  { key: "6", sub: "MNO" },
  { key: "7", sub: "PQRS" },
  { key: "8", sub: "TUV" },
  { key: "9", sub: "WXYZ" },
  { key: "*" },
  { key: "0" },
  { key: "#" },
];

export interface DialPadProps {
  onDigit: (digit: string) => void;
  onPlus: () => void;
  onBackspace: () => void;
  onCall: () => void;
  callLabel: string;
  disabled?: boolean;
  callDisabled?: boolean;
  className?: string;
  // "dial"：普通拨号；"dtmf"：通话中发送按键音。
  mode?: "dial" | "dtmf";
  callVariant?: "primary" | "danger";
}

export function DialPad({
  onDigit,
  onPlus,
  onBackspace,
  onCall,
  callLabel,
  disabled = false,
  callDisabled = false,
  className,
  mode = "dial",
  callVariant = "primary",
}: DialPadProps) {
  const dtmf = mode === "dtmf";
  return (
    <div className={cx("select-none", className)}>
      <div className="grid grid-cols-3 gap-2">
        {KEYS.map((item) => (
          <button
            key={item.key}
            type="button"
            disabled={disabled}
            onClick={() => onDigit(item.key)}
            className="flex h-12 flex-col items-center justify-center rounded-xl border border-gray-200 bg-white text-gray-800 transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:text-gray-100 dark:hover:bg-white/10 dark:active:bg-white/15"
          >
            {/* 固定两行高度：无字母的键也保留空的第二行，保证所有数字垂直对齐 */}
            <span className="flex h-5 items-center justify-center text-lg font-medium leading-none">{item.key}</span>
            <span className="flex h-3 items-center justify-center text-[9px] tracking-[0.15em] text-gray-400">
              {item.sub ?? ""}
            </span>
          </button>
        ))}
      </div>
      {!dtmf ? (
        <div className="mt-2 grid grid-cols-2 gap-2">
          <button
            type="button"
            disabled={disabled}
            onClick={onPlus}
            className="inline-flex h-10 items-center justify-center rounded-xl border border-gray-200 bg-white text-lg font-medium text-gray-800 transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:text-gray-100 dark:hover:bg-white/10"
          >
            +
          </button>
          <button
            type="button"
            disabled={disabled}
            onClick={onBackspace}
            className="inline-flex h-10 items-center justify-center rounded-xl border border-gray-200 bg-white text-lg font-medium text-gray-800 transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:text-gray-100 dark:hover:bg-white/10"
          >
            ⌫
          </button>
        </div>
      ) : null}
      <button
        type="button"
        disabled={callDisabled}
        onClick={onCall}
        className={cx(
          "mt-2 inline-flex h-11 w-full items-center justify-center rounded-xl border border-transparent text-sm font-medium text-white transition-colors disabled:cursor-not-allowed disabled:opacity-60",
          callVariant === "danger"
            ? "bg-[#f56c6c] hover:bg-[#f78989] active:bg-[#dd6161]"
            : "bg-[#67c23a] hover:bg-[#85ce61] active:bg-[#5daf34]",
        )}
      >
        {callLabel}
      </button>
    </div>
  );
}
