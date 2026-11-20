"""Sentinel platform adapter — one-way delivery into the Hermes web chat.

Sentinel (chat.denhafiz.my) is a Go web UI with its own chat transport: each
user message spawns ``hermes chat`` and the reply streams back over SSE. This
adapter therefore does NOT own inbound chat. It is a delivery sink, so the
gateway can push what it produces on its own (cron job output, notifications,
failure notices) into a Sentinel thread as a real chat message.

Transport: ``POST {SENTINEL_URL}/api/inject`` with the shared secret from
``~/.config/sentinel/inject.token`` (Sentinel generates it on first run, 0600).
Sentinel appends the message to the thread and broadcasts it on ``/api/events``,
so an open browser tab shows it immediately.

Addressing: ``deliver=sentinel:t3`` targets Sentinel thread ``t3`` (the id from
the sidebar, ``/api/state`` -> ``threads[].id``). ``deliver=sentinel`` uses
``SENTINEL_HOME_CHANNEL`` or the configured home channel.
"""

from __future__ import annotations

import json
import logging
import os
from pathlib import Path
from typing import Any, Optional

try:
    import aiohttp
    AIOHTTP_AVAILABLE = True
except ImportError:  # pragma: no cover - Hermes ships aiohttp
    AIOHTTP_AVAILABLE = False
    aiohttp = None  # type: ignore[assignment]

from gateway.config import Platform, PlatformConfig
from gateway.platforms.base import BasePlatformAdapter, SendResult
from gateway.platforms._shared import get_scoped_secret, send_error

logger = logging.getLogger(__name__)

DEFAULT_URL = "http://127.0.0.1:9121"
DEFAULT_TOKEN_FILE = "~/.config/sentinel/inject.token"
PROBE_TIMEOUT = 3.0
SEND_TIMEOUT = 15.0


def _base_url(extra: dict[str, Any]) -> str:
    url = str(extra.get("url") or get_scoped_secret("SENTINEL_URL", "") or DEFAULT_URL).strip()
    return url.rstrip("/")


def _token_path(extra: dict[str, Any]) -> str:
    raw = str(extra.get("token_file") or get_scoped_secret("SENTINEL_TOKEN_FILE", "") or DEFAULT_TOKEN_FILE)
    return os.path.expanduser(raw.strip())


def _inject_token(extra: dict[str, Any]) -> str:
    """Sentinel's shared secret: explicit extra/env first, then the token file."""
    explicit = str(extra.get("token") or get_scoped_secret("SENTINEL_INJECT_TOKEN", "") or "").strip()
    if explicit:
        return explicit
    try:
        return Path(_token_path(extra)).read_text(encoding="utf-8").strip()
    except OSError:
        return ""


def _home_thread(extra: dict[str, Any]) -> str:
    return str(extra.get("home_channel") or get_scoped_secret("SENTINEL_HOME_CHANNEL", "") or "").strip()


async def _post_inject(url: str, token: str, payload: dict[str, Any]) -> dict[str, Any]:
    """POST one delivery. Returns ``{"ok": True, "msg_id": ...}`` or ``{"error": str}``."""
    if not token:
        return {"error": f"sentinel: no inject token (looked for {DEFAULT_TOKEN_FILE})"}
    if not AIOHTTP_AVAILABLE:
        return {"error": "sentinel: aiohttp is not installed"}
    headers = {"X-Sentinel-Token": token, "Content-Type": "application/json"}
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=SEND_TIMEOUT)) as session:
            async with session.post(f"{url}/api/inject", json=payload, headers=headers) as resp:
                body = await resp.text()
                if resp.status != 200:
                    # 401 = token mismatch, 404 = thread gone, 403 = refused source
                    return {"error": f"sentinel HTTP {resp.status}: {body[:200]}"}
                try:
                    data = json.loads(body)
                except ValueError:
                    return {"ok": True}
                return {"ok": True, "msg_id": data.get("msgId"), "thread": data.get("thread")}
    except Exception as e:  # network / DNS / timeout
        return {"error": f"sentinel unreachable at {url}: {e}"}


def _payload(chat_id: str, content: str, metadata: Optional[dict[str, Any]]) -> dict[str, Any]:
    meta = metadata or {}
    payload: dict[str, Any] = {"text": content}
    if chat_id:
        payload["thread"] = chat_id
    role = str(meta.get("role") or "").strip().lower()
    if role in ("assistant", "error"):
        payload["role"] = role
    title = str(meta.get("title") or "").strip()
    if title:
        payload["title"] = title[:80]
    return payload


class SentinelAdapter(BasePlatformAdapter):
    """Delivery-only adapter for the Sentinel web chat."""

    def __init__(self, config: PlatformConfig):
        super().__init__(config=config, platform=Platform("sentinel"))
        self._extra: dict[str, Any] = dict(config.extra or {})
        self._url = _base_url(self._extra)
        self._token = _inject_token(self._extra)

    # -- lifecycle ---------------------------------------------------------
    async def connect(self, *, is_reconnect: bool = False) -> bool:
        """Probe Sentinel's /healthz. Delivery is outbound only: nothing to subscribe to."""
        if not AIOHTTP_AVAILABLE:
            logger.warning("[%s] aiohttp missing — cannot deliver", self.name)
            return False
        if not self._token:
            logger.warning(
                "[%s] no inject token at %s — deliveries will fail until Sentinel has started once",
                self.name, _token_path(self._extra))
        try:
            timeout = aiohttp.ClientTimeout(total=PROBE_TIMEOUT)
            async with aiohttp.ClientSession(timeout=timeout) as session:
                async with session.get(f"{self._url}/healthz") as resp:
                    ok = resp.status == 200
        except Exception as e:
            logger.warning("[%s] Sentinel not reachable at %s: %s", self.name, self._url, e)
            ok = False
        if not ok:
            return False
        self._mark_connected()
        logger.info("[%s] connected to %s (delivery only, home thread %s)",
                    self.name, self._url, _home_thread(self._extra) or "active")
        return True

    async def disconnect(self) -> None:
        self._mark_disconnected()

    # -- outbound ----------------------------------------------------------
    async def send(
        self, chat_id: str, content: str, reply_to: Optional[str] = None,
        metadata: Optional[dict[str, Any]] = None,
    ) -> SendResult:
        thread = (chat_id or "").strip() or _home_thread(self._extra)
        result = await _post_inject(self._url, self._token, _payload(thread, content, metadata))
        if "error" in result:
            logger.warning("[%s] delivery failed: %s", self.name, result["error"])
            return SendResult(success=False, error=result["error"])
        return SendResult(success=True, message_id=str(result.get("msg_id") or ""))

    async def get_chat_info(self, chat_id: str) -> dict[str, Any]:
        return {"name": f"Sentinel {chat_id}".strip(), "type": "dm"}


# -- plugin registration ---------------------------------------------------


def check_requirements() -> bool:
    """Passive probe: no dependencies beyond the gateway's own aiohttp."""
    return AIOHTTP_AVAILABLE


def validate_config(config: PlatformConfig) -> bool:
    extra = dict(getattr(config, "extra", {}) or {})
    return bool(_base_url(extra))


def is_configured(config: PlatformConfig) -> bool:
    """Configured = Sentinel answers /healthz (used only for env-only activation)."""
    extra = dict(getattr(config, "extra", {}) or {})
    return bool(_base_url(extra)) and bool(_inject_token(extra))


async def _standalone_send(
    pconfig, chat_id: str, message: str, *,
    thread_id: Optional[str] = None, media_files: Optional[list[str]] = None,
    force_document: bool = False, **kwargs: Any,
) -> dict[str, Any]:
    """Out-of-process delivery (cron tick without a co-resident gateway)."""
    extra = dict(getattr(pconfig, "extra", {}) or {})
    thread = (chat_id or thread_id or "").strip() or _home_thread(extra)
    result = await _post_inject(_base_url(extra), _inject_token(extra), _payload(thread, message, kwargs))
    if "error" in result:
        return send_error(result["error"])
    return {"success": True, "platform": "sentinel", "chat_id": thread,
            "message_id": str(result.get("msg_id") or "")}


def register(ctx) -> None:
    """Plugin entry point — called by the Hermes plugin system at startup."""
    ctx.register_platform(
        name="sentinel",
        label="Sentinel",
        adapter_factory=lambda cfg: SentinelAdapter(cfg),
        check_fn=check_requirements,
        validate_config=validate_config,
        is_connected=is_configured,
        required_env=[],
        install_hint="",
        cron_deliver_env_var="SENTINEL_HOME_CHANNEL",
        standalone_sender_fn=_standalone_send,
        emoji="🛰",
        allow_update_command=True,
        platform_hint=(
            "You are delivering a message into Sentinel, Hafiz's web chat at chat.denhafiz.my. "
            "It renders GitHub-flavoured markdown with syntax-highlighted code blocks, so keep "
            "formatting light and lead with the answer. Deliveries are one-way: the user replies "
            "in the web chat, not here, so never ask a question that expects an answer in this "
            "message."
        ),
    )
