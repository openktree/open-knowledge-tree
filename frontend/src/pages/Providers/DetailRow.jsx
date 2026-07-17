export default function DetailRow(props) {
  return (
    <div>
      <p class="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mb-1">
        {props.label}
      </p>
      <div>{props.children}</div>
    </div>
  );
}
