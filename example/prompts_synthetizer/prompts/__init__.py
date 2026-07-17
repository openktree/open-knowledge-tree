"""Shared LLM system prompts used across workers and the API.

All pipeline/agent prompt constants live here so that services (API,
workers) can import them without cross-service dependencies.
"""

from kt_agents_core.prompts.composite import (
    COMPOSITE_SYNTHESIS_SYSTEM_PROMPT,
    PERSPECTIVE_SYSTEM_PROMPT,
)
from kt_agents_core.prompts.definitions import DEFINITION_SYSTEM_PROMPT
from kt_agents_core.prompts.edges import EDGE_RESOLUTION_SYSTEM_PROMPT
from kt_agents_core.prompts.super_synthesizer import SUPER_SYNTHESIZER_SYSTEM_PROMPT
from kt_agents_core.prompts.synthesizer import SYNTHESIZER_SYSTEM_PROMPT

__all__ = [
    "COMPOSITE_SYNTHESIS_SYSTEM_PROMPT",
    "DEFINITION_SYSTEM_PROMPT",
    "EDGE_RESOLUTION_SYSTEM_PROMPT",
    "PERSPECTIVE_SYSTEM_PROMPT",
    "SUPER_SYNTHESIZER_SYSTEM_PROMPT",
    "SYNTHESIZER_SYSTEM_PROMPT",
]
