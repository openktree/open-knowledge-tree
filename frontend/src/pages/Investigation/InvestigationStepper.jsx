import { A, useNavigate, useParams } from "@solidjs/router";
import { For, Show } from "solid-js";
import { PHASES } from "./constants";

export default function InvestigationStepper(props) {
  const params = useParams();
  const navigate = useNavigate();
  const activePhase = () => params.phase || "sources";
  const slug = () => props.slug;
  const invID = () => props.invID;

  const phaseHref = (phaseId) => `/${slug()}/investigations/${invID()}/${phaseId}`;

  const goPhase = (phaseId, e) => {
    e.preventDefault();
    navigate(phaseHref(phaseId));
  };

  return (
    <nav class="flex items-center gap-2 flex-wrap" aria-label="Investigation phases">
      <For each={PHASES}>
        {(phase, idx) => (
          <>
            <Show when={idx() > 0}>
              <span class="text-gray-400 dark:text-gray-600">{"\u203a"}</span>
            </Show>
            <Show
              when={!phase.disabled}
              fallback={
                <span
                  class="text-sm px-3 py-1.5 rounded-full bg-gray-100 dark:bg-gray-700 text-gray-400 dark:text-gray-500 cursor-not-allowed"
                  title={phase.note || ""}
                >
                  {phase.label}
                </span>
              }
            >
              <Show
                when={activePhase() === phase.id}
                fallback={
                  <A
                    href={phaseHref(phase.id)}
                    class="text-sm px-3 py-1.5 rounded-full bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300 hover:bg-blue-50 dark:hover:bg-blue-900/30 hover:text-blue-600 dark:hover:text-blue-400 transition"
                  >
                    {phase.label}
                  </A>
                }
              >
                <span class="text-sm px-3 py-1.5 rounded-full bg-blue-600 text-white font-medium">
                  {phase.label}
                </span>
              </Show>
            </Show>
          </>
        )}
      </For>
    </nav>
  );
}
