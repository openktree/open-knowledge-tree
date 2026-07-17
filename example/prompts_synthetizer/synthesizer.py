"""System prompt for the Synthesizer Agent.

Adapted from the reference MCP synthesizer agent (old-knowledge-tree/.claude/agents/synthesizer.md)
for use as a Hatchet-based LangGraph agent that produces standalone research documents.

The canonical prompt constant lives in ``kt_agents_core.prompts.synthesizer``.
This module re-exports it for backwards compatibility and adds the builder helper.
"""

from kt_agents_core.prompts.synthesizer import SYNTHESIZER_SYSTEM_PROMPT


def build_synthesizer_system_message(topic: str, starting_node_ids: list[str], budget: int) -> str:
    """Build the full system message for a synthesis run."""
    task_block = "\n\n# YOUR TASK\n\n"
    task_block += f"## Topic\n{topic}\n\n"
    task_block += f"## Exploration Budget\nYou can visit up to {budget} nodes.\n\n"

    if starting_node_ids:
        task_block += "## Starting Nodes\nBegin your investigation from these nodes:\n"
        for nid in starting_node_ids:
            task_block += f"- {nid}\n"
        task_block += "\nUse get_node and get_edges on these first, then expand outward.\n"
    else:
        task_block += (
            "## Getting Started\nNo starting nodes provided. Use search_graph with "
            "multiple query terms to discover relevant nodes, then drill in.\n"
        )

    return SYNTHESIZER_SYSTEM_PROMPT + task_block
