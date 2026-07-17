import { createSignal } from "solid-js";

const [authToken, setAuthToken] = createSignal(localStorage.getItem("token"));

export function setToken(value) {
  if (value) {
    localStorage.setItem("token", value);
  } else {
    localStorage.removeItem("token");
  }
  setAuthToken(value);
}

export function getTokenSignal() {
  return authToken;
}

const [user, setUser] = createSignal(null);

export function useAuth() {
  const isAuthenticated = () => !!authToken();
  return { user, setUser, token: authToken, isAuthenticated };
}
