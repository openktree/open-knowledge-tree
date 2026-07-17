export default function StatCard(props) {
  return (
    <div class="bg-white dark:bg-gray-800 rounded-lg shadow-md p-6">
      <div class="flex items-center gap-3">
        <div class="p-2 bg-blue-50 dark:bg-blue-900/30 rounded-md">
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
        <div>
          <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">
            {props.label}
          </dt>
          <dd class="mt-0.5 text-2xl font-semibold text-gray-900 dark:text-gray-100">
            {props.value}
          </dd>
        </div>
      </div>
    </div>
  );
}