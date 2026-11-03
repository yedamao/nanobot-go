import { LocaleSwitcher, useT } from "./i18n";
import { SettingsCard } from "./SettingsCard";
import { ThemeSwitcher } from "./theme";

export function AppearancePage() {
  const t = useT();
  return <section className="page" aria-labelledby="appearance-title">
    <header className="page-header"><div className="page-title"><h2 id="appearance-title">{t("appearance.title")}</h2><p>{t("appearance.blurb")}</p></div></header>
    <div className="section-stack">
      <SettingsCard icon="settings" title={t("appearance.themeTitle")} help={t("appearance.themeHelp")}>
        <div className="appearance-control"><ThemeSwitcher id="appearance-theme" /></div>
      </SettingsCard>
      <SettingsCard icon="book" title={t("appearance.localeTitle")} help={t("appearance.localeHelp")}>
        <div className="appearance-control"><LocaleSwitcher id="appearance-locale" /></div>
      </SettingsCard>
    </div>
  </section>;
}
