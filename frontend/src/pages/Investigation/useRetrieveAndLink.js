import { api } from "../../services/api";

const POLL_MS = 1500;
const POLL_MAX_ATTEMPTS = 120;

export function useRetrieveAndLink({ slug, invID, onLinked }) {
  let progressSetter = null;

  const setProgressSetter = (fn) => {
    progressSetter = fn;
  };

  const setProgress = (key, value) => {
    if (progressSetter) progressSetter(key, value);
  };

  const enqueue = async (result, repoID, process = false) => {
    const key = result.url;
    setProgress(key, { stage: "fetching", process });
    try {
      const enq = await api.retrieveSource(result.url, repoID, process, result.doi || "");
      setProgress(key, { stage: "polling", jobId: enq.job_id, process });
      pollAndLink(enq.job_id, key);
    } catch (err) {
      setProgress(key, { stage: "error", error: err.message });
    }
  };

  const pollAndLink = async (jobId, resultKey, attempt = 0) => {
    if (attempt >= POLL_MAX_ATTEMPTS) {
      setProgress(resultKey, { stage: "error", error: "Timed out waiting for job" });
      return;
    }
    try {
      const job = await api.getTask(jobId);
      if (job.state === "completed") {
        await linkOrReport(job, resultKey);
        return;
      }
      if (job.state === "cancelled" || job.state === "discarded") {
        setProgress(resultKey, { stage: "error", error: `Job ${job.state}` });
        return;
      }
      setTimeout(() => pollAndLink(jobId, resultKey, attempt + 1), POLL_MS);
    } catch (err) {
      setProgress(resultKey, { stage: "error", error: err.message });
    }
  };

  const linkOrReport = async (job, resultKey) => {
    const sourceID = job.output?.source_id;
    if (!sourceID) {
      setProgress(resultKey, { stage: "error", error: "Job completed but produced no source_id" });
      return;
    }
    try {
      await api.addInvestigationSource(slug, invID, sourceID);
      onLinked?.();
      setProgress(resultKey, { stage: "done", sourceID, jobId: job.id });
    } catch (err) {
      setProgress(resultKey, { stage: "error", error: err.message });
    }
  };

  return { enqueue, setProgressSetter };
}