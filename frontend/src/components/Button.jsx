const variantClasses = {
  primary: "bg-primary text-white hover:bg-primary-hover",
  active: "bg-primary text-white cursor-default",
  secondary: "bg-surface text-text-muted hover:text-text-base border border-border",
  danger: "bg-danger text-white hover:opacity-90",
  ghost: "text-danger hover:opacity-80",
  link: "text-link hover:underline",
};

export default function Button(props) {
  return (
    <button
      type={props.type || "button"}
      onClick={props.onClick}
      disabled={props.disabled || props.loading}
      class={`px-4 py-2 rounded transition text-sm font-medium disabled:opacity-50 focus:outline-none focus:ring-2 focus:ring-primary-ring ${variantClasses[props.variant || "primary"]} ${props.class || ""}`}
    >
      {props.loading ? (props.loadingText || "Loading...") : props.children}
    </button>
  );
}