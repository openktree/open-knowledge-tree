import { A } from "@solidjs/router";
import { createSignal, For, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import CitedView from "../../components/CitedView";
import { buildCitedText } from "../../lib/citedCopy";
import { extractInlineCitations } from "../../lib/extractInlineCitations";
import { api } from "../../services/api";
import { formatScore, formatTimestamp, statusVariant } from "./constants";

export default function ReportDetailContent(props) {
  const report = () => props.report() || {};
  const [copied, setCopied] = createSignal(false);
  const [copiedCites, setCopiedCites] = createSignal(false);
  const [copyingCites, setCopyingCites] = createSignal(false);

  const copyToClipboard = async (text) => {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
    }
  };

  const handleCopy = async () => {
    try {
      await copyToClipboard(report().body_md ?? "");
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message || "Failed to copy report" });
    }
  };

  const handleCopyCites = async () => {
    const anns = props.annotations() || [];
    const body = report().body_md ?? "";
    setCopyingCites(true);
    try {
      // Collect ALL inline-cited fact ids (from the author's own
      // [text](<fact:uuid>) links). These go in the "direct cites"
      // section of the annex FIRST and take priority over auto-
      // annotated facts (a fact that is both inline-cited and auto-
      // matched appears only as a direct cite, not duplicated).
      // Auto-only facts (annotated but not inline-cited) are fetched
      // for their sources only (text comes from the annotation row).
      const annIds = new Set(anns.map((a) => a.fact_id));
      const { factIds: inlineIds } = extractInlineCitations(body);
      const inlineIdSet = new Set(inlineIds);
      const autoOnlyIds = [...annIds].filter((id) => !inlineIdSet.has(id));

      const factSources = new Map();
      const inlineFacts = new Map();
      await Promise.all(
        [...new Set([...inlineIds, ...autoOnlyIds])].map(async (fid) => {
          try {
            const res = await api.getFact(props.slug, fid);
            const sources = res.sources || [];
            if (inlineIdSet.has(fid)) {
              // Inline-cited: needs text + sources for the direct section.
              inlineFacts.set(fid, {
                text: res.fact?.text || "",
                sources,
                error: null,
              });
              factSources.set(fid, sources);
            } else {
              // Auto-only: needs sources only (text from annotation row).
              factSources.set(fid, sources);
            }
          } catch (err) {
            // Non-fatal: record the error per-fact so the annex
            // surfaces it and the copy still succeeds.
            if (inlineIdSet.has(fid)) {
              inlineFacts.set(fid, {
                text: "",
                sources: [],
                error: err?.message || "fetch failed",
              });
            } else {
              factSources.set(fid, []);
            }
          }
        }),
      );
      const text = buildCitedText(body, anns, factSources, inlineFacts);
      if (!anns.length && inlineIds.length === 0) {
        props.onAlert?.({ variant: "error", message: "No citations to include." });
        return;
      }
      await copyToClipboard(text);
      setCopiedCites(true);
      setTimeout(() => setCopiedCites(false), 1500);
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message || "Failed to copy cited report" });
    } finally {
      setCopyingCites(false);
    }
  };

  return (
    <div class="space-y-6">
      <Show when={report().error}>
        <Alert variant="error" message={`Annotation failed: ${report().error}`} />
      </Show>
      <Card>
        <div class="flex items-start justify-between gap-4 mb-4">
          <div>
            <h1 class="text-xl font-bold dark:text-white">{report().title}</h1>
            <Show when={report().topic}>
              <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">{report().topic}</p>
            </Show>
          </div>
          <div class="flex gap-2">
            <Button variant="secondary" onClick={handleCopy}>
              {copied() ? "Copied" : "Copy"}
            </Button>
            <Button variant="secondary" onClick={handleCopyCites} loading={copyingCites()}>
              {copiedCites() ? "Copied" : "Copy with cites"}
            </Button>
            <Button variant="secondary" onClick={props.onRefresh}>
              Refresh
            </Button>
            <Show when={props.canUpdate()}>
              <Button variant="primary" onClick={props.onAnnotate} loading={props.annotating()}>
                Re-annotate
              </Button>
            </Show>
          </div>
        </div>
        <div class="flex flex-wrap gap-4 text-xs text-gray-500 dark:text-gray-400 mb-4">
          <span
            class={`inline-block px-2 py-0.5 rounded font-medium ${statusVariant[report().status] || ""}`}
          >
            {report().status}
          </span>
          <Show when={report().sentence_count != null}>
            <span>{report().sentence_count} sentences</span>
          </Show>
          <Show when={report().similarity_threshold != null}>
            <span>threshold {formatScore(report().similarity_threshold)}</span>
          </Show>
          <Show when={report().embedded_model}>
            <span>model: {report().embedded_model}</span>
          </Show>
          <Show when={report().annotation_job_id}>
            <span>job: {report().annotation_job_id}</span>
          </Show>
          <span>created {formatTimestamp(report().created_at)}</span>
        </div>
        <Show when={props.parentReport?.()}>
          <div class="text-xs text-gray-500 dark:text-gray-400 mb-2">
            Part of{" "}
            <A
              href={`/${props.slug}/reports/${props.parentReport().id}`}
              class="text-blue-600 dark:text-blue-400 hover:underline"
            >
              {props.parentReport().title}
            </A>
          </div>
        </Show>
        <Show when={(props.children?.() || []).length > 0}>
          <div class="text-xs text-gray-500 dark:text-gray-400">
            Sub-reports:
            <ul class="mt-1 space-y-0.5">
              <For each={props.children()}>
                {(child) => (
                  <li>
                    <A
                      href={`/${props.slug}/reports/${child.id}`}
                      class="text-blue-600 dark:text-blue-400 hover:underline"
                    >
                      {child.title}
                    </A>
                    <Show when={child.topic}>
                      <span class="text-gray-400 dark:text-gray-500"> — {child.topic}</span>
                    </Show>
                  </li>
                )}
              </For>
            </ul>
          </div>
        </Show>
      </Card>
      <Show when={report().body_md}>
        <Card>
          <div class="border-b border-gray-200 dark:border-gray-700 pb-3 mb-3">
            <p class="text-xs text-gray-500 dark:text-gray-400">
              Annotated report — highlighted sentences have matching facts.
              <Show when={props.annotations().length > 0}> Click to view.</Show>
            </p>
          </div>
          <CitedView
            markdown={report().body_md}
            annotations={props.annotations()}
            slug={props.slug}
            onSentenceClick={props.onSentenceClick}
          />
        </Card>
      </Show>
    </div>
  );
}
