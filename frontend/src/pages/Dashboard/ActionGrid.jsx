import { For } from "solid-js";
import ActionCard from "./ActionCard";

export default function ActionGrid(props) {
  return (
    <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
      <For each={props.cards}>{(card) => <ActionCard {...card} />}</For>
    </div>
  );
}
