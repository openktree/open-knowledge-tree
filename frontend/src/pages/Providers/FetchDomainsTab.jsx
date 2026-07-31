import { For, Show } from "solid-js";
import Alert from "../../components/Alert";
import HostFailureMatrix from "./HostFailureMatrix";
import HostSkipProviders from "./HostSkipProviders";

/**
 * FetchDomainsTab is the dedicated tab for per-host fetch
 * diagnostics and the learned/manual skip list. The merged
 * HostFailureMatrix table shows per-host failure/success
 * counts by provider tier with a per-row dropdown to set the
 * chain entry point or ban a host. HostSkipProviders keeps the
 * explicit add/remove/clear controls. All diagnostics are
 * repo-scoped via the X-Repository-ID header the api client
 * injects; when no repo is selected the cards render their
 * empty-state fallbacks so the tab is never blank.
 *
 * Props:
 *   providers: () => the /sources/providers resource
 *   refetch: () => Promise<void> refetch the providers resource
 *     after a skip mutation so the skip list refreshes.
 */
export default function FetchDomainsTab(props) {
  const matrix = () => (props.providers() && props.providers().host_failure_matrix) || [];
  const hostSkips = () => (props.providers() && props.providers().host_skip_providers) || [];

  const onChanged = async () => {
    if (props.refetch) await props.refetch();
  };

  return (
    <div>
      <Alert
        variant="info"
        message="Per-host fetch diagnostics and the skip list are scoped to the active repository. Set a chain entry point per host to skip weaker tiers, or ban a host entirely. Select a repository to populate the table."
      />

      <HostFailureMatrix matrix={matrix} skips={hostSkips} onChanged={onChanged} />

      <HostSkipProviders skips={hostSkips} onChanged={onChanged} />
    </div>
  );
}
