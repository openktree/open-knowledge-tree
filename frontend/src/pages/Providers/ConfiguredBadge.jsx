import { Show } from "solid-js";
import Badge from "../../components/Badge";

export default function ConfiguredBadge(props) {
  return (
    <Show
      when={props.configured}
      fallback={
        <Badge variant="red" title={props.requires ? `requires ${props.requires}` : undefined}>
          not configured
        </Badge>
      }
    >
      <Badge variant="green">active</Badge>
    </Show>
  );
}
