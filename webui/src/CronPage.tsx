import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  createCronJob,
  listCronJobs,
  patchCronJob,
  removeCronJob,
  runCronJob,
  type CronJob,
  type CronWrite,
} from "./api";

function fmt(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

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
}

function emptyForm(): FormState {
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
  };
}

function buildWrite(form: FormState, forCreate: boolean): CronWrite | string {
  const message = form.message.trim();
  if (!message) return "message is required";
  const write: CronWrite = {
    message,
    enabled: form.enabled,
  };
  const name = form.name.trim();
  if (name || !forCreate) write.name = name;
  if (form.kind === "every") {
    const mins = Number(form.everyMinutes);
    if (!Number.isFinite(mins) || mins <= 0) return "every needs a positive interval (minutes)";
    write.schedule = { kind: "every", everyMs: Math.round(mins * 60000) };
  } else if (form.kind === "cron") {
    const expr = form.expr.trim();
    if (!expr) return "cron schedule needs an expression";
    write.schedule = { kind: "cron", expr, tz: form.tz.trim() || undefined };
  } else {
    if (!form.atLocal.trim()) return "one-time schedule needs a date/time";
    const d = new Date(form.atLocal);
    if (Number.isNaN(d.getTime())) return "invalid date/time";
    write.schedule = { kind: "at", at: d.toISOString() };
    write.deleteAfterRun = form.deleteAfterRun || forCreate;
  }
  if (form.kind !== "at" && !forCreate) {
    write.deleteAfterRun = form.deleteAfterRun;
  }
  return write;
}

export function CronPage() {
  const [jobs, setJobs] = useState<CronJob[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [create, setCreate] = useState<FormState>(emptyForm);
  const [editing, setEditing] = useState<CronJob | null>(null);
  const [editForm, setEditForm] = useState<FormState>(emptyForm);

  const load = useCallback(async () => {
    setError("");
    try {
      setJobs(await listCronJobs());
    } catch (err) {
      setJobs(null);
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const run = async (job: CronJob) => {
    setBusy(job.id);
    setNote("");
    try {
      await runCronJob(job.id, !job.enabled);
      setNote("Ran " + job.name);
      await load();
    } catch (err) {
      setNote("Run failed: " + (err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const remove = async (job: CronJob) => {
    if (job.system) return;
    if (!confirm('Remove cron job "' + job.name + '"?')) return;
    setBusy(job.id);
    setNote("");
    try {
      await removeCronJob(job.id);
      setNote("Removed " + job.name);
      await load();
    } catch (err) {
      setNote("Remove failed: " + (err as Error).message);
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
      setNote((job.enabled ? "Disabled " : "Enabled ") + job.name);
      await load();
    } catch (err) {
      setNote("Update failed: " + (err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const submitCreate = async (e: FormEvent) => {
    e.preventDefault();
    const write = buildWrite(create, true);
    if (typeof write === "string") {
      setNote(write);
      return;
    }
    setBusy("create");
    setNote("");
    try {
      const job = await createCronJob(write);
      setNote("Created " + job.name);
      setCreate(emptyForm());
      await load();
    } catch (err) {
      setNote("Create failed: " + (err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const openEdit = (job: CronJob) => {
    if (job.system) return;
    setEditing(job);
    setEditForm(formFromJob(job));
    setNote("");
  };

  const submitEdit = async (e: FormEvent) => {
    e.preventDefault();
    if (!editing) return;
    const write = buildWrite(editForm, false);
    if (typeof write === "string") {
      setNote(write);
      return;
    }
    setBusy(editing.id);
    setNote("");
    try {
      const job = await patchCronJob(editing.id, write);
      setNote("Updated " + job.name);
      setEditing(null);
      await load();
    } catch (err) {
      setNote("Update failed: " + (err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="page" aria-labelledby="cron-heading">
      <header className="page-header">
        <h2 id="cron-heading">Cron</h2>
        <button type="button" onClick={() => void load()} disabled={busy !== null}>
          Refresh
        </button>
      </header>
      {error && <p className="page-error">Cannot load cron jobs ({error}).</p>}
      {note && <p className="page-note">{note}</p>}

      <form className="cron-form" onSubmit={(e) => void submitCreate(e)}>
        <h3>New job</h3>
        <CronFields form={create} setForm={setCreate} idPrefix="create" />
        <div className="cron-form-actions">
          <button type="submit" disabled={busy !== null}>
            Create
          </button>
        </div>
      </form>

      {!error && jobs === null && <p className="page-empty">Loading…</p>}
      {!error && jobs && jobs.length === 0 && (
        <p className="page-empty">No scheduled jobs yet. Create one above.</p>
      )}
      {jobs && jobs.length > 0 && (
        <table className="page-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Schedule</th>
              <th>Next</th>
              <th>Last</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((j) => (
              <tr key={j.id} data-system={j.system || undefined} data-disabled={!j.enabled || undefined}>
                <td>
                  {j.name}
                  {j.system && <span className="badge">system</span>}
                  {!j.enabled && <span className="badge">disabled</span>}
                </td>
                <td>{j.schedule.summary}</td>
                <td>{fmt(j.state.nextRun)}</td>
                <td>{fmt(j.state.lastRun)}</td>
                <td>
                  {j.state.lastStatus || "—"}
                  {j.state.lastError ? " (" + j.state.lastError + ")" : ""}
                </td>
                <td className="page-actions">
                  <button type="button" disabled={busy === j.id} onClick={() => void run(j)}>
                    Run
                  </button>
                  <button type="button" disabled={j.system || busy === j.id} onClick={() => openEdit(j)}>
                    Edit
                  </button>
                  <button type="button" disabled={j.system || busy === j.id} onClick={() => void toggleEnabled(j)}>
                    {j.enabled ? "Disable" : "Enable"}
                  </button>
                  <button type="button" disabled={j.system || busy === j.id} onClick={() => void remove(j)}>
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {editing && (
        <dialog className="cron-dialog" open onCancel={() => setEditing(null)}>
          <form className="cron-form" onSubmit={(e) => void submitEdit(e)}>
            <h3>Edit {editing.name}</h3>
            <CronFields form={editForm} setForm={setEditForm} idPrefix="edit" />
            <div className="cron-form-actions">
              <button type="button" onClick={() => setEditing(null)} disabled={busy === editing.id}>
                Cancel
              </button>
              <button type="submit" disabled={busy === editing.id}>
                Save
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
}: {
  form: FormState;
  setForm: (f: FormState) => void;
  idPrefix: string;
}) {
  const set = (patch: Partial<FormState>) => setForm({ ...form, ...patch });
  return (
    <fieldset className="cron-fields">
      <label htmlFor={idPrefix + "-name"}>
        Name
        <input
          id={idPrefix + "-name"}
          type="text"
          value={form.name}
          placeholder="optional"
          onChange={(e) => set({ name: e.target.value })}
        />
      </label>
      <label htmlFor={idPrefix + "-message"}>
        Message
        <textarea
          id={idPrefix + "-message"}
          rows={3}
          required
          value={form.message}
          placeholder="What the agent should do"
          onChange={(e) => set({ message: e.target.value })}
        />
      </label>
      <label htmlFor={idPrefix + "-kind"}>
        Schedule
        <select
          id={idPrefix + "-kind"}
          value={form.kind}
          onChange={(e) => set({ kind: e.target.value as Kind })}
        >
          <option value="every">Every N minutes</option>
          <option value="cron">Cron expression</option>
          <option value="at">One-time (at)</option>
        </select>
      </label>
      {form.kind === "every" && (
        <label htmlFor={idPrefix + "-every"}>
          Minutes
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
            Expression
            <input
              id={idPrefix + "-expr"}
              type="text"
              value={form.expr}
              placeholder="0 9 * * 1-5"
              onChange={(e) => set({ expr: e.target.value })}
            />
          </label>
          <label htmlFor={idPrefix + "-tz"}>
            Timezone
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
            When
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
            Delete after run
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
        Enabled
      </label>
    </fieldset>
  );
}
