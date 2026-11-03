import type { ReactNode } from "react";

export function Switch({ id, checked, onChange, label }: { id?: string; checked: boolean; onChange: (value: boolean) => void; label: string }) {
  return <label className="switch" title={label}>
    <input id={id} type="checkbox" checked={checked} aria-label={label} onChange={(event) => onChange(event.target.checked)} />
    <span />
  </label>;
}

export function Row({ label, help, htmlFor, top, children }: { label: ReactNode; help?: ReactNode; htmlFor?: string; top?: boolean; children: ReactNode }) {
  return <div className={"form-row" + (top ? " top" : "")}>
    <label htmlFor={htmlFor}>{label}{help && <span className="help">{help}</span>}</label>
    <div className="ctl">{children}</div>
  </div>;
}
