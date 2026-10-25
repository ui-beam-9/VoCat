import { useCallback } from "react";
import { OIDCFlowDialog } from "./OIDCFlowDialog";
import { useI18n } from "../../lib/i18n";
import { startOIDCBinding } from "../../api";

interface OIDCBindDialogProps {
  open: boolean;
  onClose: () => void;
  /** 绑定成功后的回调：父组件用它重新拉取绑定状态。 */
  onBound: () => void;
  /** 由父组件在点击手势内同步打开的授权弹出窗口。 */
  popup: Window | null;
  /** 重新打开授权弹出窗口（必须在点击手势内调用，否则会被弹窗拦截）。 */
  onOpenPopup: () => Window | null;
}

// 绑定 OIDC 用户：授权流程由通用 OIDCFlowDialog 承担，这里只注入绑定语义。
export function OIDCBindDialog({
  open,
  onClose,
  onBound,
  popup,
  onOpenPopup,
}: OIDCBindDialogProps) {
  const { t } = useI18n();
  const start = useCallback(async () => (await startOIDCBinding()).authorizationUrl, []);
  return (
    <OIDCFlowDialog
      open={open}
      onClose={onClose}
      popup={popup}
      onOpenPopup={onOpenPopup}
      start={start}
      successEvent="vocat:oidc-bound"
      successMessage={t("OIDC 用户绑定成功")}
      errorFallback={t("OIDC 用户绑定失败")}
      onSuccess={onBound}
      title={t("绑定 OIDC 用户")}
    />
  );
}
