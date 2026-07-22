import { useSearchParams } from "@solidjs/router";
import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Layout from "../../components/Layout";
import Tabs from "../../components/Tabs";
import { api } from "../../services/api";
import { getTokenSignal } from "../../store/auth";
import CreateTokenModal from "./CreateTokenModal";
import ProfileInfo from "./ProfileInfo";
import TokensTable from "./TokensTable";

/**
 * Profile — the user's account page. Two tabs: Profile (edit display
 * name) and API Tokens (create/list/revoke personal access tokens).
 * Accessible from the top-right user dropdown in the header, which
 * can deep-link to a specific tab via ?tab=profile|tokens.
 *
 * Data: user + tokens are createResource'd from the session token
 * signal; mutations refetch the relevant resource.
 */
export default function Profile() {
  const token = getTokenSignal();
  const [searchParams, setSearchParams] = useSearchParams();
  const initialTab = () => (searchParams.tab === "tokens" ? "tokens" : "profile");
  const [tab, setTab] = createSignal(initialTab());
  const [alert, setAlert] = createSignal(null);
  const [modalOpen, setModalOpen] = createSignal(false);

  const changeTab = (next) => {
    setTab(next);
    // Keep the URL in sync so a refresh preserves the active tab and
    // the menu's "API Tokens" deep link lands on the right tab.
    setSearchParams({ tab: next }, { replace: true });
  };

  const [user, { refetch: refetchUser }] = createResource(token, (t) => (t ? api.getMe() : null));
  const [keys, { refetch: refetchKeys }] = createResource(token, (t) =>
    t ? api.listApiKeys().catch(() => ({ api_keys: [] })) : { api_keys: [] },
  );
  const [repositories] = createResource(token, (t) =>
    t ? api.listRepositories().catch(() => ({ repositories: [] })) : { repositories: [] },
  );

  const handleUpdateProfile = async (body) => {
    const u = user();
    if (!u) throw new Error("no user");
    await api.updateProfile(u.id, body);
    refetchUser();
  };

  const handleCreate = async (payload) => {
    const result = await api.createApiKey(payload);
    refetchKeys();
    setAlert({ variant: "success", message: `Token "${payload.name}" created` });
    return result;
  };

  const handleRevoke = async (keyID) => {
    try {
      await api.revokeApiKey(keyID);
      setAlert({ variant: "success", message: "Token revoked" });
      refetchKeys();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    }
  };

  return (
    <Layout>
      <div class="space-y-6">
        <Alert
          variant={alert()?.variant}
          message={alert()?.message}
          onDismiss={() => setAlert(null)}
        />

        <Tabs
          tabs={[
            { value: "profile", label: "Profile" },
            { value: "tokens", label: "API Tokens" },
          ]}
          active={tab()}
          onChange={changeTab}
        />

        <Show when={tab() === "profile"}>
          <ProfileInfo user={user} onUpdateProfile={handleUpdateProfile} onAlert={setAlert} />
        </Show>

        <Show when={tab() === "tokens"}>
          <div class="space-y-4">
            <div class="flex justify-end">
              <Button onClick={() => setModalOpen(true)}>Create token</Button>
            </div>
            <TokensTable
              keys={() => keys()?.api_keys}
              repositories={() => repositories()?.repositories || []}
              onRevoke={handleRevoke}
              onAlert={setAlert}
            />
          </div>
        </Show>

        <CreateTokenModal
          open={modalOpen()}
          onClose={() => setModalOpen(false)}
          repositories={() => repositories()?.repositories || []}
          onCreate={handleCreate}
          onAlert={setAlert}
        />
      </div>
    </Layout>
  );
}
