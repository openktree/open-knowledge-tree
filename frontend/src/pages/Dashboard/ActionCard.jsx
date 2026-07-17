export default function ActionCard(props) {
  return (
    <a
      href={props.href}
      class="group block bg-white dark:bg-gray-800 rounded-lg shadow-md p-6 hover:shadow-lg transition"
    >
      <div class="flex items-start gap-3">
        <div class="p-2 bg-blue-50 dark:bg-blue-900/30 rounded-md flex-shrink-0">
          <svg
            xmlns="http://www.w3.org/2000/svg"
            class="h-5 w-5 text-blue-600 dark:text-blue-400"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <path d={props.icon} />
          </svg>
        </div>
        <div class="flex-1 min-w-0">
          <h3 class="text-base font-semibold text-gray-900 dark:text-gray-100">
            {props.title}
          </h3>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {props.description}
          </p>
          <span class="mt-3 inline-flex items-center gap-1 text-sm font-medium text-blue-600 dark:text-blue-400 group-hover:gap-2 transition-all">
            {props.cta}
            <svg
              xmlns="http://www.w3.org/2000/svg"
              class="h-4 w-4"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="M5 12h14M12 5l7 7-7 7" />
            </svg>
          </span>
        </div>
      </div>
    </a>
  );
}