import { cx } from "../../lib/utils";

// 电话号码键盘：1-9 / * / 0 / #，外加 +、退格与呼叫按钮。
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
}: DialPadProps) {
  return (
    <div className={cx("select-none", className)}>
      <div className="grid grid-cols-3 gap-2">
        {KEYS.map((item) => (
          <button
            key={item.key}
            type="button"
            disabled={disabled}
            onClick={() => onDigit(item.key)}
            className="flex h-12 flex-col items-center justify-center rounded-xl border border-gray-200 bg-white text-lg font-medium text-gray-800 transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:text-gray-100 dark:hover:bg-white/10 dark:active:bg-white/15"
          >
            <span className="leading-none">{item.key}</span>
            {item.sub ? (
              <span className="mt-0.5 text-[9px] tracking-[0.15em] text-gray-400">{item.sub}</span>
            ) : null}
          </button>
        ))}
      </div>
      <div className="mt-2 grid grid-cols-2 gap-2">
        <button
          type="button"
          disabled={disabled}
          onClick={onPlus}
          className="h-10 rounded-xl border border-gray-200 bg-white text-lg font-medium text-gray-800 transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:text-gray-100 dark:hover:bg-white/10"
        >
          +
        </button>
        <button
          type="button"
          disabled={disabled}
          onClick={onBackspace}
          className="h-10 rounded-xl border border-gray-200 bg-white text-lg font-medium text-gray-800 transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:text-gray-100 dark:hover:bg-white/10"
        >
          ⌫
        </button>
      </div>
      <button
        type="button"
        disabled={callDisabled}
        onClick={onCall}
        className="mt-2 h-11 w-full rounded-xl border border-transparent bg-[#67c23a] text-sm font-medium text-white transition-colors hover:bg-[#85ce61] active:bg-[#5daf34] disabled:cursor-not-allowed disabled:opacity-60"
      >
        {callLabel}
      </button>
    </div>
  );
}
