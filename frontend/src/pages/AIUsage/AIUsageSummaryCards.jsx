import Card from "../../components/Card";
import { formatNumber, formatCost } from "./constants";

// AIUsageSummaryCards renders the top-of-dashboard stat cards.
// Input / Output tokens are shown separately because pricing
// differs per axis and the split is the first thing an operator
// wants when diagnosing cost. Total tokens is kept as a third
// card for quick cross-check; cost and requests round out the row.
export default function AIUsageSummaryCards(props) {
  const totalPrompt = () => props.summary?.total_prompt_tokens ?? 0;
  const totalCompletion = () => props.summary?.total_completion_tokens ?? 0;
  const totalTokens = () => props.summary?.total_tokens ?? 0;
  const totalCost = () => props.summary?.total_cost ?? 0;
  const requests = () =>
    (props.summary?.rows ?? []).reduce((acc, r) => acc + (r.request_count ?? 0), 0);

  return (
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-5 gap-4">
      <StatCard label="Input Tokens" value={formatNumber(totalPrompt())} />
      <StatCard label="Output Tokens" value={formatNumber(totalCompletion())} />
      <StatCard label="Total Tokens" value={formatNumber(totalTokens())} />
      <StatCard label="Est. Cost" value={formatCost(totalCost())} />
      <StatCard label="Requests" value={formatNumber(requests())} />
    </div>
  );
}

function StatCard(props) {
  return (
    <Card padding="md">
      <p class="text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wide">
        {props.label}
      </p>
      <p class="mt-1 text-2xl font-semibold text-gray-900 dark:text-white">
        {props.value}
      </p>
    </Card>
  );
}