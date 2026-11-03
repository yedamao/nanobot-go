import type { ReactNode } from "react";
import { Icon, type IconName } from "./Icon";

export function SettingsCard({ icon, title, help, meta, children }: { icon?: IconName; title: ReactNode; help: ReactNode; meta?: ReactNode; children: ReactNode }) {
  return <article className="form-card settings-card">
    <div className="settings-card-head">
      {icon && <span className="icon-wrap"><Icon name={icon} /></span>}
      <div className="settings-card-title"><h3>{title}</h3><p>{help}</p></div>
      {meta && <div className="head-meta">{meta}</div>}
    </div>
    {children}
  </article>;
}
