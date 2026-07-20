const variantClasses = {
  red: "bg-danger/10 text-danger",
  purple: "bg-primary/10 text-primary-fg",
  blue: "bg-info/10 text-info",
  green: "bg-success/10 text-success",
  gray: "bg-border text-text-muted",
  yellow: "bg-warning/10 text-warning",
  primary: "bg-primary/10 text-primary-fg",
  success: "bg-success/10 text-success",
  info: "bg-info/10 text-info",
  warning: "bg-warning/10 text-warning",
  danger: "bg-danger/10 text-danger",
  neutral: "bg-border text-text-muted",
};

export default function Badge(props) {
  return (
    <span
      class={`text-xs px-2 py-0.5 rounded font-mono ${variantClasses[props.variant] || variantClasses.neutral} ${props.class || ""}`}
    >
      {props.children}
    </span>
  );
}
