import { createContext, useContext, createResource } from "solid-js";
import { api } from "../services/api";
import { getTokenSignal } from "./auth";

const RBACContext = createContext();

export function RBACProvider(props) {
  const token = getTokenSignal();

  const [permsData, { mutate }] = createResource(
    () => !!token(),
    async () => {
      if (!token()) return { permissions: [], system_admin: false };
      try {
        return await api.getMyPermissions();
      } catch {
        return { permissions: [], system_admin: false };
      }
    }
  );

  const value = [permsData, mutate];

  return (
    <RBACContext.Provider value={value}>
      {props.children}
    </RBACContext.Provider>
  );
}

export function useRBAC() {
  const val = useContext(RBACContext);
  if (!val) {
    const dead = { permissions: () => [], systemAdmin: () => false, loaded: () => false, loading: () => false, hasPermission: () => false, refresh: async () => {} };
    return dead;
  }

  const [permsData, mutate] = val;

  const perms = () => {
    const d = permsData();
    return d ? d.permissions || [] : [];
  };
  const admin = () => {
    const d = permsData();
    return d ? d.system_admin || false : false;
  };
  const loaded = () => permsData.state === "ready";
  const loading = () => permsData.loading;

  return {
    permissions: perms,
    systemAdmin: admin,
    loaded,
    loading,
    hasPermission(resource, action) {
      if (admin()) return true;
      const p = perms();
      return p.some(
        (x) => x.resource === resource && (x.action === action || x.action === "*")
      ) || p.some(
        (x) => x.resource === "*" && x.action === "*"
      );
    },
    async refresh() {
      const data = await api.getMyPermissions();
      mutate(data);
    },
  };
}
