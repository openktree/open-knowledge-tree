export default function InfoBanner() {
  return (
    <div class="bg-blue-50 dark:bg-blue-900 border border-blue-200 dark:border-blue-700 rounded-lg p-5 shadow-lg">
      <div class="flex gap-3">
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-5 w-5 text-blue-600 dark:text-blue-400 flex-shrink-0 mt-0.5"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
        </svg>
        <div class="flex-1">
          <h3 class="text-sm font-semibold text-blue-900 dark:text-blue-200">
            Investigations are your main manual research tool
          </h3>
          <p class="mt-1.5 text-sm text-blue-800 dark:text-blue-300 leading-relaxed">
            Create an investigation around a research topic, then choose sources to fetch. Fetched
            sources are decomposed into facts, and those facts become concepts over time. Concepts
            populate the graph, surfacing connections you can explore for further research.
          </p>
        </div>
      </div>
    </div>
  );
}
