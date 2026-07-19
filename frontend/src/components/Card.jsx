const paddingClasses = {
  sm: "p-4",
  md: "p-6",
  lg: "p-8",
};

export default function Card(props) {
  return (
    <div
      class={`bg-surface border border-border rounded-lg shadow-card dark:shadow-card-dark ${paddingClasses[props.padding || "md"]} ${props.class || ""}`}
    >
      {props.children}
    </div>
  );
}
