import { For, Show } from "solid-js";
import Alert from "../../components/Alert";
import FetchProviderCard from "./FetchProviderCard";
import ChainOverview from "./ChainOverview";

export default function FetchProvidersTab(props) {
  const fetchProviders = () =>
    (props.providers() && props.providers().resolution) || [];

  return (
    <div>
      <Alert
        variant="info"
        message="Fetch providers are tried in priority order. The first one that supports the source type and returns successfully wins; the rest are skipped. The HTTP Fetch provider always runs last as a catch-all."
      />

      <ChainOverview providers={fetchProviders} />

      <Show when={fetchProviders().length > 0}>
        <For each={fetchProviders()}>
          {(p) => <FetchProviderCard provider={p} />}
        </For>
      </Show>
    </div>
  );
}
