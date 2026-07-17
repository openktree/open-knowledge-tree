// useNowTicker returns a signal that updates once per second
// with the current time, and stops ticking when the document
// is hidden. The Tasks table uses it to render a live "elapsed
// since attempted_at" counter for running and retryable jobs
// without spawning an interval per row.
//
// The hook owns the interval. The caller reads now() inside
// any reactive context and the result re-renders on each tick.
// On dispose (component unmount, route change) the interval is
// cleared and the visibilitychange listener is removed so we
// don't leak timers or listeners across navigations.
import { createSignal, createEffect, onCleanup } from "solid-js";

export function useNowTicker() {
  const [now, setNow] = createSignal(Date.now());
  let intervalID = null;

  const start = () => {
    if (intervalID != null) return;
    intervalID = setInterval(() => setNow(Date.now()), 1000);
  };
  const stop = () => {
    if (intervalID != null) {
      clearInterval(intervalID);
      intervalID = null;
    }
  };

  createEffect(() => {
    const onVis = () => {
      if (document.hidden) {
        stop();
      } else {
        setNow(Date.now());
        start();
      }
    };
    onVis();
    document.addEventListener("visibilitychange", onVis);
    onCleanup(() => {
      document.removeEventListener("visibilitychange", onVis);
      stop();
    });
  });

  return now;
}

// resolveJobDuration returns the millisecond value the table
// should display for a job, or null when the job has no
// measurable execution time yet. The backend sends
// duration_ms for terminal jobs (completed / cancelled /
// discarded) and a snapshot for live jobs (running /
// retryable); for live jobs we always prefer the local clock
// so the cell keeps ticking between server refreshes.
//
//   - null: server says no work has happened yet (job
//     never attempted, no attempted_at, no duration).
//     Render as "—".
//   - integer: render through formatDurationMs.
//
// The function is a pure helper. It takes the job object
// and the current now() value from useNowTicker so the
// caller can keep the reactivity on the cell, not on the
// helper.
export function resolveJobDuration(job, now) {
  if (!job) return null;
  const liveStates = job.state === "running" || job.state === "retryable";
  if (liveStates && job.attempted_at) {
    return now - new Date(job.attempted_at).getTime();
  }
  if (job.duration_ms != null) return job.duration_ms;
  return null;
}
