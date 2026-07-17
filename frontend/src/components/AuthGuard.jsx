import { onMount, Show } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { getTokenSignal } from "../store/auth";
import Loading from "./Loading";

export default function AuthGuard(props) {
  const navigate = useNavigate();
  const token = getTokenSignal();

  onMount(() => {
    if (!token()) {
      navigate("/login", { replace: true });
    }
  });

  return (
    <Show when={!!token()} fallback={<Loading />}>
      {props.children}
    </Show>
  );
}
