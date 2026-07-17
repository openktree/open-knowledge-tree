"""System prompt for the SuperSynthesizer Agent.

The canonical prompt constant lives in ``kt_agents_core.prompts.super_synthesizer``.
This module re-exports it for backwards compatibility and adds the builder helper.
"""

from kt_agents_core.prompts.super_synthesizer import SUPER_SYNTHESIZER_SYSTEM_PROMPT


def build_super_synthesizer_system_message(
    topic: str,
    synthesis_node_ids: list[str],
) -> str:
    """Build the full system message for a super-synthesis run."""
    task_block = "\n\n# YOUR TASK\n\n"
    task_block += f"## Topic\n{topic}\n\n"
    task_block += "## Sub-Syntheses to Combine\n"
    task_block += f"You have {len(synthesis_node_ids)} sub-synthesis documents to read and combine:\n"
    for nid in synthesis_node_ids:
        task_block += f"- {nid}\n"
    task_block += (
        "\nRead ALL of them first using read_synthesis, then produce your super-synthesis "
        "using finish_super_synthesis(text).\n"
    )
    return SUPER_SYNTHESIZER_SYSTEM_PROMPT + task_block
