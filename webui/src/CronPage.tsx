import { Fragment, useCallback, useEffect, useState, type FormEvent } from "react";
import {
  createCronJob,
  listCronJobs,
  listCronRuns,
  listCronTargets,
  patchCronJob,
  removeCronJob,
  runCronJob,
  type CronJob,
  type CronRun,
  type CronTargets,
  type CronWrite,
} from "./api";
import {
  CUSTOM,
  DEFAULT_BINDING,
  NONE,
  bindingKey,
  bindingLabel,
  bindingOptions,
  bindingWrite,
  optionLabel,
  type Binding,
  type BindingOption,
} from "./cronBinding";
import { formatDuration, runReply, runStatusState, runStatusText } from "./cronRuns";
import { useFormatDate, useT, type MessageKey } from "./i18n";

type Kind = "every" | "cron" | "at";

interface FormState {
  name: string;
  message: string;
  kind: Kind;
  everyMinutes: string;
  expr: string;
  tz: string;
  atLocal: string;
  enabled: boolean;
  deleteAfterRun: boolean;
  // binding is the chat the job delivers to; undefined only for an
  // edited job that has none. custom shows the channel/chat id inputs.
  binding?: Binding;
  custom: boolean;
}

function emptyForm(binding: Binding = DEFAULT_BINDING): FormState {
  return {
    name: "",
    message: "",
    kind: "every",
    everyMinutes: "60",
    expr: "0 9 * * *",
    tz: "Asia/Shanghai",
    atLocal: "",
    enabled: true,
    deleteAfterRun: false,
    binding,
    custom: false,
  };
}

function formFromJob(job: CronJob): FormState {
  const kind = (job.schedule.kind as Kind) || "every";
  let everyMinutes = "60";
  if (kind === "every" && job.schedule.everyMs && job.schedule.everyMs > 0) {
    everyMinutes = String(Math.max(1, Math.round(job.schedule.everyMs / 60000)));
  }
  let atLocal = "";
  if (kind === "at" && job.schedule.at) {
    const d = new Date(job.schedule.at);
    if (!Number.isNaN(d.getTime())) {
      const pad = (n: number) => String(n).padStart(2, "0");
      atLocal =
        d.getFullYear() +
        "-" +
        pad(d.getMonth() + 1) +
        "-" +
        pad(d.getDate()) +
        "T" +
        pad(d.getHours()) +
        ":" +
        pad(d.getMinutes());
    }
  }
  return {
    name: job.name,
    message: job.payload.message ?? "",
    kind: kind === "at" || kind === "cron" || kind === "every" ? kind : "every",
    everyMinutes,
    expr: job.schedule.expr || "0 9 * * *",
    tz: job.schedule.tz || "Asia/Shanghai",
    atLocal,
    enabled: job.enabled,
    deleteAfterRun: !!job.deleteAfterRun,
    binding: job.binding,
    custom: false,
  };
}

// buildWrite returns the request, or the message key of what is missing.
// base is the binding the server keeps when none is sent: the default
// on create, the job's own on edit.
function buildWrite(form: FormState, forCreate: boolean, base: Binding | undefined): CronWrite | MessageKey {
  const message = form.message.trim();
  if (!message) return "cron.validation.messageRequired";
  const write: CronWrite = {
    message,
    enabled: form.enabled,
  };
  const name = form.name.trim();
  if (name || !forCreate) write.name = name;
  if (form.kind === "every") {
    const mins = Number(form.everyMinutes);
    if (!Number.isFinite(mins) || mins <= 0) return "cron.validation.everyPositive";
    write.schedule = { kind: "every", everyMs: Math.round(mins * 60000) };
  } else if (form.kind === "cron") {
    const expr = form.expr.trim();
    if (!expr) return "cron.validation.exprRequired";
    write.schedule = { kind: "cron", expr, tz: form.tz.trim() || undefined };
  } else {
    if (!form.atLocal.trim()) return "cron.validation.atRequired";
    const d = new Date(form.atLocal);
    if (Number.isNaN(d.getTime())) return "cron.validation.atInvalid";
    write.schedule = { kind: "at", at: d.toISOString() };
    write.deleteAfterRun = form.deleteAfterRun || forCreate;
  }
  if (form.kind !== "at" && !forCreate) {
    write.deleteAfterRun = form.deleteAfterRun;
  }
  const binding = bindingWrite(form.binding, base);
  if (typeof binding === "string") return binding;
  if (binding) write.binding = binding;
  return write;
}

export function CronPage() {
  const formatDate = useFormatDate();
  const fmt = (iso?: string) => (iso ? formatDate(iso) : "—");
  const [jobs, setJobs] = useState<CronJob[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [create, setCreate] = useState<FormState>(emptyForm);
  const [editing, setEditing] = useState<CronJob | null>(null);
  const [editForm, setEditForm] = useState<FormState>(emptyForm);
  const [targets, setTargets] = useState<CronTargets | null>(null);
  const [targetsError, setTargetsError] = useState("");
  const [history, setHistory] = useState<{ job: CronJob; runs: CronRun[] | null; error: string } | null>(null);
  const t = useT();

  const load = useCallback(async () => {
    setError("");
    try {
      setJobs(await listCronJobs());
    } catch (err) {
      setJobs(null);
      setError((err as Error).message);
    }
  }, []);

  const loadTargets = useCallback(async () => {
    try {
      setTargets(await listCronTargets());
      setTargetsError("");
    } catch (err) {
      setTargetsError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
    void loadTargets();
  }, [load, loadTargets]);

  const defaultBinding = targets?.default ?? DEFAULT_BINDING;

  const loadHistory = async (job: CronJob) => {
    setHistory({ job, runs: null, error: "" });
    try {
      const { runs } = await listCronRuns(job.id);
      setHistory((h) => (h && h.job.id === job.id ? { job, runs, error: "" } : h));
    } catch (err) {
      setHistory((h) => (h && h.job.id === job.id ? { job, runs: null, error: (err as Error).message } : h));
    }
  };

  const run = async (job: CronJob) => {
    setBusy(job.id);
    setNote("");
    try {
      await runCronJob(job.id, !job.enabled);
      setNote(t("cron.notes.ran", { name: job.name }));
      await load();
      if (history?.job.id === job.id) await loadHistory(job);
    } catch (err) {
      setNote(t("cron.notes.runFailed", { error: (err as Error).message }));
    } finally {
      setBusy(null);
    }
  };

  const remove = async (job: CronJob) => {
    if (job.system) return;
    if (!confirm(t("cron.notes.removeConfirm", { name: job.name }))) return;
    setBusy(job.id);
    setNote("");
    try {
      await removeCronJob(job.id);
      setNote(t("cron.notes.removed", { name: job.name }));
      await load();
    } catch (err) {
      setNote(t("cron.notes.removeFailed", { error: (err as Error).message }));
    } finally {
      setBusy(null);
    }
  };

  const toggleEnabled = async (job: CronJob) => {
    if (job.system) return;
    setBusy(job.id);
    setNote("");
    try {
      await patchCronJob(job.id, { enabled: !job.enabled });
      setNote(t(job.enabled ? "cron.notes.disabled" : "cron.notes.enabled", { name: job.name }));
      await load();
    } catch (err) {
      setNote(t("cron.notes.updateFailed", { error: (err as Error).message }));
    } finally {
      setBusy(null);
    }
  };

  const submitCreate = async (e: FormEvent) => {
    e.preventDefault();
    const write = buildWrite(create, true, defaultBinding);
    if (typeof write === "string") {
      setNote(t(write));
      return;
    }
    setBusy("create");
    setNote("");
    try {
      const job = await createCronJob(write);
      setNote(t("cron.notes.created", { name: job.name }));
      setCreate(emptyForm(defaultBinding));
      await load();
    } catch (err) {
      setNote(t("cron.notes.createFailed", { error: (err as Error).message }));
    } finally {
      setBusy(null);
    }
  };

  const openEdit = (job: CronJob) => {
    if (job.system) return;
    setEditing(job);
    setEditForm(formFromJob(job));
    setNote("");
    void loadTargets();
  };

  const submitEdit = async (e: FormEvent) => {
    e.preventDefault();
    if (!editing) return;
    const write = buildWrite(editForm, false, editing.binding);
    if (typeof write === "string") {
      setNote(t(write));
      return;
    }
    setBusy(editing.id);
    setNote("");
    try {
      const job = await patchCronJob(editing.id, write);
      setNote(t("cron.notes.updated", { name: job.name }));
      setEditing(null);
      await load();
    } catch (err) {
      setNote(t("cron.notes.updateFailed", { error: (err as Error).message }));
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="page" aria-labelledby="cron-heading">
      <header className="page-header">
        <h2 id="cron-heading">{t("cron.heading")}</h2>
        <button
          type="button"
          onClick={() => {
            void load();
            void loadTargets();
          }}
          disabled={busy !== null}
        >
          {t("common.refresh")}
        </button>
      </header>
      {error && <p className="page-error">{t("cron.loadError", { error })}</p>}
      {note && <p className="page-note">{note}</p>}

      <form className="cron-form" onSubmit={(e) => void submitCreate(e)}>
        <h3>{t("cron.newJob")}</h3>
        <CronFields
          form={create}
          setForm={setCreate}
          idPrefix="create"
          options={bindingOptions(targets?.targets ?? [], defaultBinding)}
          defaultBinding={defaultBinding}
          targetsError={targetsError}
        />
        <div className="cron-form-actions">
          <button type="submit" disabled={busy !== null}>
            {t("cron.create")}
          </button>
        </div>
      </form>

      {!error && jobs === null && <p className="page-empty">{t("common.loading")}</p>}
      {!error && jobs && jobs.length === 0 && (
        <p className="page-empty">{t("cron.empty")}</p>
      )}
      {jobs && jobs.length > 0 && (
        <table className="page-table">
          <thead>
            <tr>
              <th>{t("cron.columns.name")}</th>
              <th>{t("cron.columns.schedule")}</th>
              <th>{t("cron.columns.binding")}</th>
              <th>{t("cron.columns.next")}</th>
              <th>{t("cron.columns.last")}</th>
              <th>{t("cron.columns.status")}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((j) => (
              <tr key={j.id} data-system={j.system || undefined} data-disabled={!j.enabled || undefined}>
                <td>
                  {j.name}
                  {j.system && <span className="badge">{t("cron.badges.system")}</span>}
                  {!j.enabled && <span className="badge">{t("cron.badges.disabled")}</span>}
                </td>
                <td>{j.schedule.summary}</td>
                <td className="cron-binding">
                  {j.system
                    ? t("cron.binding.system")
                    : j.binding
                      ? bindingLabel(j.binding)
                      : t("cron.binding.none")}
                </td>
                <td>{fmt(j.state.nextRun)}</td>
                <td>{fmt(j.state.lastRun)}</td>
                <td>
                  {runStatusText(j.state.lastStatus, t)}
                  {j.state.lastError ? " (" + j.state.lastError + ")" : ""}
                </td>
                <td className="page-actions">
                  <button type="button" disabled={busy === j.id} onClick={() => void run(j)}>
                    {t("cron.actions.run")}
                  </button>
                  <button type="button" onClick={() => void loadHistory(j)}>
                    {t("cron.actions.history")}
                  </button>
                  <button type="button" disabled={j.system || busy === j.id} onClick={() => openEdit(j)}>
                    {t("cron.actions.edit")}
                  </button>
                  <button type="button" disabled={j.system || busy === j.id} onClick={() => void toggleEnabled(j)}>
                    {j.enabled ? t("cron.actions.disable") : t("cron.actions.enable")}
                  </button>
                  <button type="button" disabled={j.system || busy === j.id} onClick={() => void remove(j)}>
                    {t("cron.actions.remove")}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {history && (
        <dialog className="cron-dialog cron-history" open onCancel={() => setHistory(null)}>
          <div className="cron-form">
            <h3>{t("cron.history.title", { name: history.job.name })}</h3>
            {history.error && <p className="page-error">{t("cron.history.loadError", { error: history.error })}</p>}
            {!history.error && history.runs === null && <p className="page-empty">{t("common.loading")}</p>}
            {history.runs && history.runs.length === 0 && <p className="page-empty">{t("cron.history.empty")}</p>}
            {history.runs && history.runs.length > 0 && (
              <table className="page-table">
                <thead>
                  <tr>
                    <th>{t("cron.history.columns.time")}</th>
                    <th>{t("cron.history.columns.status")}</th>
                    <th>{t("cron.history.columns.duration")}</th>
                    <th>{t("cron.history.columns.error")}</th>
                  </tr>
                </thead>
                <tbody>
                  {history.runs.map((r, i) => {
                    const reply = runReply(r);
                    return (
                      <Fragment key={r.runAt + ":" + i}>
                        <tr className="cron-run-row">
                          <td>{fmt(r.runAt)}</td>
                          <td>
                            <span className="badge" data-state={runStatusState(r.status)}>
                              {runStatusText(r.status, t)}
                            </span>
                          </td>
                          <td>{formatDuration(r.durationMs)}</td>
                          <td className="cron-run-error">{r.error || ""}</td>
                        </tr>
                        <tr className="cron-run-reply-row">
                          <td colSpan={4}>
                            {reply ? (
                              <pre className="cron-run-reply">{reply}</pre>
                            ) : (
                              <span className="cron-run-noreply">{t("cron.history.noReply")}</span>
                            )}
                          </td>
                        </tr>
                      </Fragment>
                    );
                  })}
                </tbody>
              </table>
            )}
            <div className="cron-form-actions">
              <button type="button" onClick={() => void loadHistory(history.job)}>
                {t("common.refresh")}
              </button>
              <button type="button" onClick={() => setHistory(null)}>
                {t("common.close")}
              </button>
            </div>
          </div>
        </dialog>
      )}

      {editing && (
        <dialog className="cron-dialog" open onCancel={() => setEditing(null)}>
          <form className="cron-form" onSubmit={(e) => void submitEdit(e)}>
            <h3>{t("cron.edit.title", { name: editing.name })}</h3>
            <CronFields
              form={editForm}
              setForm={setEditForm}
              idPrefix="edit"
              options={bindingOptions(targets?.targets ?? [], defaultBinding, editing.binding)}
              defaultBinding={defaultBinding}
              targetsError={targetsError}
            />
            <div className="cron-form-actions">
              <button type="button" onClick={() => setEditing(null)} disabled={busy === editing.id}>
                {t("common.cancel")}
              </button>
              <button type="submit" disabled={busy === editing.id}>
                {t("common.save")}
              </button>
            </div>
          </form>
        </dialog>
      )}
    </section>
  );
}

function CronFields({
  form,
  setForm,
  idPrefix,
  options,
  defaultBinding,
  targetsError,
}: {
  form: FormState;
  setForm: (f: FormState) => void;
  idPrefix: string;
  options: BindingOption[];
  defaultBinding: Binding;
  targetsError: string;
}) {
  const set = (patch: Partial<FormState>) => setForm({ ...form, ...patch });
  const t = useT();
  const bindingValue = form.custom ? CUSTOM : form.binding ? bindingKey(form.binding) : NONE;
  const pickBinding = (value: string) => {
    if (value === CUSTOM) {
      set({ custom: true, binding: form.binding ?? { channel: "", chatId: "" } });
      return;
    }
    set({ custom: false, binding: options.find((o) => bindingKey(o) === value) });
  };
  const defaultKey = bindingKey(defaultBinding);
  return (
    <fieldset className="cron-fields">
      <label htmlFor={idPrefix + "-name"}>
        {t("cron.fields.name")}
        <input
          id={idPrefix + "-name"}
          type="text"
          value={form.name}
          placeholder={t("cron.fields.namePlaceholder")}
          onChange={(e) => set({ name: e.target.value })}
        />
      </label>
      <label htmlFor={idPrefix + "-message"}>
        {t("cron.fields.message")}
        <textarea
          id={idPrefix + "-message"}
          rows={3}
          required
          value={form.message}
          placeholder={t("cron.fields.messagePlaceholder")}
          onChange={(e) => set({ message: e.target.value })}
        />
      </label>
      <label htmlFor={idPrefix + "-kind"}>
        {t("cron.fields.schedule")}
        <select
          id={idPrefix + "-kind"}
          value={form.kind}
          onChange={(e) => set({ kind: e.target.value as Kind })}
        >
          <option value="every">{t("cron.fields.kindEvery")}</option>
          <option value="cron">{t("cron.fields.kindCron")}</option>
          <option value="at">{t("cron.fields.kindAt")}</option>
        </select>
      </label>
      {form.kind === "every" && (
        <label htmlFor={idPrefix + "-every"}>
          {t("cron.fields.minutes")}
          <input
            id={idPrefix + "-every"}
            type="number"
            min={1}
            step={1}
            value={form.everyMinutes}
            onChange={(e) => set({ everyMinutes: e.target.value })}
          />
        </label>
      )}
      {form.kind === "cron" && (
        <>
          <label htmlFor={idPrefix + "-expr"}>
            {t("cron.fields.expression")}
            <input
              id={idPrefix + "-expr"}
              type="text"
              value={form.expr}
              placeholder="0 9 * * 1-5"
              onChange={(e) => set({ expr: e.target.value })}
            />
          </label>
          <label htmlFor={idPrefix + "-tz"}>
            {t("cron.fields.timezone")}
            <input
              id={idPrefix + "-tz"}
              type="text"
              value={form.tz}
              placeholder="Asia/Shanghai"
              onChange={(e) => set({ tz: e.target.value })}
            />
          </label>
        </>
      )}
      {form.kind === "at" && (
        <>
          <label htmlFor={idPrefix + "-at"}>
            {t("cron.fields.when")}
            <input
              id={idPrefix + "-at"}
              type="datetime-local"
              value={form.atLocal}
              onChange={(e) => set({ atLocal: e.target.value })}
            />
          </label>
          <label className="check" htmlFor={idPrefix + "-dar"}>
            <input
              id={idPrefix + "-dar"}
              type="checkbox"
              checked={form.deleteAfterRun}
              onChange={(e) => set({ deleteAfterRun: e.target.checked })}
            />
            {t("cron.fields.deleteAfterRun")}
          </label>
        </>
      )}
      <label htmlFor={idPrefix + "-binding"}>
        {t("cron.fields.binding")}
        <select id={idPrefix + "-binding"} value={bindingValue} onChange={(e) => pickBinding(e.target.value)}>
          {!form.custom && !form.binding && <option value={NONE}>{t("cron.fields.bindingNone")}</option>}
          {options.map((o) => {
            const key = bindingKey(o);
            return (
              <option key={key} value={key}>
                {key === defaultKey ? t("cron.fields.bindingDefault", { label: optionLabel(o) }) : optionLabel(o)}
              </option>
            );
          })}
          <option value={CUSTOM}>{t("cron.fields.bindingCustom")}</option>
        </select>
      </label>
      {targetsError && <p className="page-error">{t("cron.fields.bindingTargetsError", { error: targetsError })}</p>}
      {form.custom && (
        <>
          <label htmlFor={idPrefix + "-binding-channel"}>
            {t("cron.fields.bindingChannel")}
            <input
              id={idPrefix + "-binding-channel"}
              type="text"
              value={form.binding?.channel ?? ""}
              placeholder="telegram"
              onChange={(e) => set({ binding: { channel: e.target.value, chatId: form.binding?.chatId ?? "" } })}
            />
          </label>
          <label htmlFor={idPrefix + "-binding-chat"}>
            {t("cron.fields.bindingChatId")}
            <input
              id={idPrefix + "-binding-chat"}
              type="text"
              value={form.binding?.chatId ?? ""}
              onChange={(e) => set({ binding: { channel: form.binding?.channel ?? "", chatId: e.target.value } })}
            />
          </label>
        </>
      )}
      <label className="check" htmlFor={idPrefix + "-enabled"}>
        <input
          id={idPrefix + "-enabled"}
          type="checkbox"
          checked={form.enabled}
          onChange={(e) => set({ enabled: e.target.checked })}
        />
        {t("cron.fields.enabled")}
      </label>
    </fieldset>
  );
}
