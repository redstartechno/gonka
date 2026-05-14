"""Load-test inference requests to a Gonka node.

Requires Python 3.10+.
Run with NODE_URL and optional GONKA_PRIVATE_KEY, NUM_REQUESTS, CONCURRENCY, DEBUG.
"""
from __future__ import annotations

import json
import logging
import os
import threading
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from types import SimpleNamespace
from typing import Any, NamedTuple

import gonka_openai.utils as utils
import gonka_openai.gonka_openai as go_mod
from gonka_openai import GonkaOpenAI


class InferenceResult(NamedTuple):
    """Result of a single inference request (success or failure)."""

    index: int  # 1-based request index
    prompt: str
    text: str | None  # response content, or None on failure
    error: str | None  # error message on failure, or None on success
    inference_id: str


# --- Patch: empty endpoint list fallback (run before GonkaOpenAI) ---
def _patch_gonka_empty_endpoints(fallback_url: str | None) -> None:
    """Patch gonka_base_url to use fallback_url when endpoint list is empty."""
    if not fallback_url:
        return
    if "://" not in fallback_url:
        fallback_url = "http://" + fallback_url.lstrip("/")
    fallback_url = fallback_url.rstrip("/")

    def _fetch_identity_address(base_url: str) -> str:
        try:
            req = urllib.request.Request(base_url + "/v1/identity")
            with urllib.request.urlopen(req, timeout=10) as resp:
                data = json.loads(resp.read().decode())
                # API: {"data": {"address": "...", ...}, "signature": "..."}
                inner = data.get("data") or {}
                return (
                    inner.get("address")
                    or data.get("address")
                    or inner.get("operator_address")
                    or ""
                )
        except Exception as e:
            logging.debug(
                "Identity fetch failed for %s: %s", base_url, e, exc_info=DEBUG
            )
            return ""

    _original = utils.gonka_base_url

    def _wrapped(endpoint_list: list[str] | None = None) -> Any:
        if endpoint_list is None:
            endpoint_list = []
        if not endpoint_list and fallback_url:
            url = fallback_url.rstrip("/") + "/v1"
            address = _fetch_identity_address(fallback_url)
            return SimpleNamespace(url=url, address=address, transfer_address=address)
        return _original(endpoint_list)

    utils.gonka_base_url = _wrapped
    go_mod.gonka_base_url = _wrapped


DEBUG = os.environ.get("DEBUG", "").lower() in ("1", "true", "yes")

source_url = os.environ.get("NODE_URL", "http://89.169.110.61:8000").strip().rstrip("/")
if source_url and "://" not in source_url:
    source_url = "http://" + source_url
_patch_gonka_empty_endpoints(source_url)

client = GonkaOpenAI(
    gonka_private_key=os.environ.get("GONKA_PRIVATE_KEY"),
    source_url=source_url,
)


def _env_int(key: str, default: int) -> int:
    """Parse env key as int; return default on missing or invalid value."""
    try:
        return int(os.environ.get(key, str(default)))
    except (TypeError, ValueError):
        return default


MODEL = "Qwen/Qwen3-4B-Instruct-2507"
NUM_REQUESTS = _env_int("NUM_REQUESTS", 1000)
CONCURRENCY = max(1, _env_int("CONCURRENCY", 1))

# Slightly varied prompts: different subjects and small twists
PROMPT_TEMPLATES = [
    "Write a one-sentence bedtime story about a {subject}.",
    "In one sentence, describe a friendly {subject}.",
    "Tell me one short fact about {subject}.",
    "Complete in one sentence: Once upon a time, there was a {subject} who...",
    "One sentence: what would a {subject} say to a child?",
]
SUBJECTS = [
    "unicorn", "dragon", "cat", "robot", "bear", "owl", "fox", "whale",
    "butterfly", "elephant", "penguin", "dolphin", "rabbit", "eagle", "turtle", "wolf",
]


def make_prompt(i: int) -> str:
    """Return a prompt string for request index i (varied by template and subject)."""
    tpl = PROMPT_TEMPLATES[i % len(PROMPT_TEMPLATES)]
    subj = SUBJECTS[i % len(SUBJECTS)]
    return tpl.format(subject=subj)


def _get_inference_id_from_response(response: Any) -> str:
    """Extract inference ID from chat completion response.

    The API returns it as top-level "id" in the JSON body (OpenAI-compatible).
    SDK may expose it as .id, ["id"], or in model_dump().

    Returns:
        The inference ID string, or "" if not found.
    """
    # Prefer standard attribute (OpenAI / gonka_openai)
    if hasattr(response, "id") and response.id is not None:
        return response.id if isinstance(response.id, str) else str(response.id)
    # Dict-like (e.g. model_dump() or raw dict)
    if hasattr(response, "get"):
        val = response.get("id")
        if val is not None:
            return val if isinstance(val, str) else str(val)
    return ""


def _run_one(i: int) -> InferenceResult:
    """Execute one inference request.

    Returns:
        InferenceResult with index i+1; text/error set by success or failure.
    """
    prompt = make_prompt(i)
    try:
        response = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": prompt}],
            max_tokens=150,
        )
        text = (response.choices[0].message.content or "").strip()
        inference_id = _get_inference_id_from_response(response)
        return InferenceResult(i + 1, prompt, text, None, inference_id)
    except Exception as e:
        logging.warning(
            "Inference request failed (request %s/%s): %s",
            i + 1,
            NUM_REQUESTS,
            e,
            exc_info=DEBUG,
        )
        return InferenceResult(i + 1, prompt, None, str(e), "")


if __name__ == "__main__":
    logging.basicConfig(
        level=logging.INFO, format="%(levelname)s:%(name)s:%(message)s"
    )

    results: list[InferenceResult] = []
    if CONCURRENCY == 1:
        for i in range(NUM_REQUESTS):
            r = _run_one(i)
            results.append(r)
            if r.text is not None:
                print(
                    f"[{r.index}/{NUM_REQUESTS}] OK: {r.prompt[:50]}... "
                    f"-> {r.text[:60]}..."
                )
            else:
                print(
                    f"[{r.index}/{NUM_REQUESTS}] FAIL: {r.prompt[:50]}... "
                    f"-> {r.error}"
                )
    else:
        print_lock = threading.Lock()
        with ThreadPoolExecutor(max_workers=CONCURRENCY) as executor:
            futures = [executor.submit(_run_one, i) for i in range(NUM_REQUESTS)]
            for future in as_completed(futures):
                try:
                    r = future.result()
                except Exception as e:
                    logging.exception("Worker failed: %s", e)
                    raise
                results.append(r)
                with print_lock:
                    if r.text is not None:
                        print(
                            f"[{r.index}/{NUM_REQUESTS}] OK: {r.prompt[:50]}... "
                            f"-> {r.text[:60]}..."
                        )
                    else:
                        print(
                            f"[{r.index}/{NUM_REQUESTS}] FAIL: {r.prompt[:50]}... "
                            f"-> {r.error}"
                        )

    ok = sum(1 for r in results if r.text is not None)
    print(f"\nDone: {ok}/{NUM_REQUESTS} succeeded, {NUM_REQUESTS - ok} failed.")
