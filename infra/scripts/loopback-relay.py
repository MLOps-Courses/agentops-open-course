#!/usr/bin/env python3
"""Relay Docker-bridge TCP connections to host-loopback course services."""

from __future__ import annotations

import argparse
import asyncio
import logging
import signal
from collections.abc import Sequence
from functools import partial
from pathlib import Path

LOGGER = logging.getLogger("agentops.loopback-relay")


async def _copy_stream(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    try:
        while data := await reader.read(64 * 1024):
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.CancelledError):
        pass
    finally:
        if writer.can_write_eof():
            try:
                writer.write_eof()
                await writer.drain()
            except ConnectionError:
                pass


async def _relay_connection(
    client_reader: asyncio.StreamReader,
    client_writer: asyncio.StreamWriter,
    *,
    target_host: str,
    target_port: int,
) -> None:
    try:
        upstream_reader, upstream_writer = await asyncio.open_connection(target_host, target_port)
    except OSError as error:
        LOGGER.warning("upstream connection failed for %s:%d: %s", target_host, target_port, error)
        client_writer.close()
        await client_writer.wait_closed()
        return

    try:
        async with asyncio.TaskGroup() as tasks:
            tasks.create_task(_copy_stream(client_reader, upstream_writer))
            tasks.create_task(_copy_stream(upstream_reader, client_writer))
    finally:
        upstream_writer.close()
        client_writer.close()
        await asyncio.gather(
            upstream_writer.wait_closed(),
            client_writer.wait_closed(),
            return_exceptions=True,
        )


def _write_ready_file(path: Path, listen_host: str, ports: Sequence[int], token: str) -> None:
    temporary = path.with_suffix(".tmp")
    temporary.write_text(
        f"listen_host={listen_host}\nports={','.join(str(port) for port in ports)}\ntoken={token}\n",
        encoding="utf-8",
    )
    temporary.replace(path)


async def _serve(args: argparse.Namespace) -> None:
    ports = tuple(dict.fromkeys(args.port))
    if len(ports) != len(args.port):
        raise ValueError("relay ports must be unique")

    servers: list[asyncio.Server] = []
    try:
        for port in ports:
            server = await asyncio.start_server(
                partial(_relay_connection, target_host=args.target_host, target_port=port),
                args.listen_host,
                port,
            )
            servers.append(server)

        _write_ready_file(args.ready_file, args.listen_host, ports, args.token)
        LOGGER.info(
            "relay ready on %s for ports %s to %s loopback",
            args.listen_host,
            ",".join(str(port) for port in ports),
            args.target_host,
        )

        stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for stop_signal in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(stop_signal, stop.set)
        await stop.wait()
    finally:
        for server in servers:
            server.close()
        await asyncio.gather(*(server.wait_closed() for server in servers), return_exceptions=True)


def _parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Bind selected ports on Docker's bridge gateway and relay them to host loopback.",
    )
    parser.add_argument("--listen-host", required=True)
    parser.add_argument("--target-host", default="127.0.0.1")
    parser.add_argument("--port", action="append", required=True, type=int)
    parser.add_argument("--ready-file", required=True, type=Path)
    parser.add_argument("--token", required=True)
    return parser.parse_args(argv)


def main(argv: Sequence[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    asyncio.run(_serve(_parse_args(argv)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
