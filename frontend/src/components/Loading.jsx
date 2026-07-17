export default function Loading(props) {
  return (
    <div class={`min-h-screen flex items-center justify-center bg-page ${props.class || ""}`}>
      <p class="text-text-muted">{props.message || "Loading..."}</p>
    </div>
  );
}