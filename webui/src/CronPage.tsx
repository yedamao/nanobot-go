import { useCallback, useEffect, useState } from "react";
import { listCronJobs, removeCronJob, runCronJob, type CronJob } from "./api";

function fmt(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

export function CronPage() {
  const [jobs, setJobs] = useState<CronJob[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState("");

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
      {!error && jobs === null && <p className="page-empty">Loading…</p>}
      {!error && jobs && jobs.length === 0 && (
        <p className="page-empty">No scheduled jobs. Ask the agent to create one, or use nanobot cron.</p>
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
                  <button type="button" disabled={j.system || busy === j.id} onClick={() => void remove(j)}>
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
