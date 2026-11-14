#!/usr/bin/env python3
"""Optional Windows loopback -> WSL NAT IPv4 -> Service TCP bridge.

WSL's ordinary localhost forwarding creates a Hyper-V socket for each new
connection. This helper uses the VM's existing Ethernet path instead. WSL is
invoked once to start a small Linux peer, never for individual HTTP requests.
Both peers are unprivileged and use only Python's standard library. See
docs/windows-localhost.md before using this optional, NAT-only workaround.
"""
import argparse
import asyncio
import contextlib
import ipaddress
import json
import os
import pathlib
import socket
import subprocess
import sys
import time


def emit(event, **fields):
    # Only connection metadata/counters go to the log, never application bytes.
    print(json.dumps(dict(event=event, unix=time.time(), **fields)), flush=True)


def private_ipv4(value):
    ip = ipaddress.IPv4Address(value)
    if not ip.is_private or ip.is_loopback or ip.is_unspecified or ip.is_link_local:
        raise ValueError("expected a private, non-loopback NAT IPv4 address")
    return str(ip)


def port_number(value):
    port = int(value)
    if not 1 <= port <= 65535:
        raise argparse.ArgumentTypeError("port must be between 1 and 65535")
    return port


def linux_addresses():
    """Use explicit eth0 IPv4 binding; 0.0.0.0 would enable WSL forwarding again.

    In NAT mode the Windows host is the default gateway. Reject other peers
    before opening a connection to the app. This is an access restriction, not
    an authentication system for a remotely accessible service.
    """
    addresses = json.loads(subprocess.check_output(
        ["ip", "-4", "-j", "address", "show", "dev", "eth0"], timeout=5))
    routes = json.loads(subprocess.check_output(
        ["ip", "-4", "-j", "route", "show", "default"], timeout=5))
    ips = [a["local"] for link in addresses for a in link.get("addr_info", [])
           if a.get("family") == "inet" and a.get("scope") == "global"]
    gateways = [r["gateway"] for r in routes if r.get("dev") == "eth0" and r.get("gateway")]
    if len(ips) != 1 or len(gateways) != 1:
        raise ValueError("requires one eth0 IPv4 address and one NAT default gateway")
    return private_ipv4(ips[0]), private_ipv4(gateways[0])


class TCPForwarder:
    """Copy a TCP stream once, without interpreting or retrying HTTP.

    A failed POST must never be replayed: the app might already have committed
    it. A failure therefore closes that connection and increments a counter.
    Two pumps allow simultaneous upload/download. EOF in one direction sends
    FIN only in that direction, so a response after an upload is not truncated.
    Backpressure and a connection limit bound memory use for slow clients.
    """

    def __init__(self, target, *, allowed_peer=None, max_connections=128, connect_timeout=3):
        self.target = target
        self.allowed_peer = allowed_peer
        self.max_connections = max_connections
        self.connect_timeout = connect_timeout
        self.server = None
        self.closing = False
        self.tasks = set()
        self.counts = dict(accepted=0, completed=0, errors=0, rejected=0, cancelled=0,
                           peakActive=0, bytesUp=0, bytesDown=0, retries=0)

    async def start(self, address):
        # Windows SO_REUSEADDR can steal a listener from another application.
        # An exclusive bind fails rather than displacing an existing service.
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            if os.name == "nt":
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
            else:
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(address)
            sock.listen(128)
            sock.setblocking(False)
            self.server = await asyncio.start_server(self.handle, sock=sock, limit=65536)
        except BaseException:
            sock.close()
            raise
        return self.server.sockets[0].getsockname()

    async def pump(self, reader, writer, counter):
        while chunk := await reader.read(65536):
            writer.write(chunk)
            await writer.drain()
            self.counts[counter] += len(chunk)
        if writer.can_write_eof():
            writer.write_eof()
            await writer.drain()

    @staticmethod
    async def close_writer(writer):
        if writer is not None:
            writer.close()
            with contextlib.suppress(OSError, TimeoutError):
                await asyncio.wait_for(writer.wait_closed(), 1)

    async def handle(self, reader, writer):
        peer = writer.get_extra_info("peername")
        if self.closing or (self.allowed_peer is not None and (not peer or peer[0] != self.allowed_peer)) or len(self.tasks) >= self.max_connections:
            self.counts["rejected"] += 1
            await self.close_writer(writer)
            return
        current = asyncio.current_task()
        self.tasks.add(current)
        self.counts["accepted"] += 1
        self.counts["peakActive"] = max(self.counts["peakActive"], len(self.tasks))
        upstream = None
        pumps = []
        try:
            remote, upstream = await asyncio.wait_for(
                asyncio.open_connection(*self.target, limit=65536), self.connect_timeout)
            pumps = [asyncio.create_task(self.pump(reader, upstream, "bytesUp")),
                     asyncio.create_task(self.pump(remote, writer, "bytesDown"))]
            await asyncio.gather(*pumps)
            self.counts["completed"] += 1
        except asyncio.CancelledError:
            self.counts["cancelled"] += 1
            raise
        except (OSError, TimeoutError):
            self.counts["errors"] += 1
        finally:
            for task in pumps:
                task.cancel()
            await asyncio.gather(*pumps, return_exceptions=True)
            await self.close_writer(upstream)
            await self.close_writer(writer)
            self.tasks.discard(current)

    async def close(self, grace=10):
        # First stop accepting, then let existing connections finish. The
        # grace limit also makes cleanup bounded when a peer never sends EOF.
        self.closing = True
        if self.server:
            self.server.close()
        pending = set(self.tasks)
        if pending:
            _, pending = await asyncio.wait(pending, timeout=grace)
            for task in pending:
                task.cancel()
            await asyncio.gather(*pending, return_exceptions=True)
        if self.server:
            # Supported Python versions may wait for accepted connections too. Waiting before
            # draining/cancelling their handlers would deadlock shutdown.
            await self.server.wait_closed()


async def linux_peer(args):
    if sys.platform != "linux" or not os.environ.get("WSL_DISTRO_NAME") or os.geteuid() == 0:
        raise ValueError("Linux peer must run as an ordinary WSL user")
    bind_ip, gateway = linux_addresses()
    if (args.target_ip, args.target_port) == (bind_ip, args.linux_port):
        raise ValueError("Linux target must not point back to this listener")
    bridge = TCPForwarder((args.target_ip, args.target_port), allowed_peer=gateway)
    await bridge.start((bind_ip, args.linux_port))
    emit("linux-ready", pid=os.getpid(), bindIP=bind_ip, bindPort=args.linux_port, allowedPeer=gateway)
    # The Windows parent owns this peer through a single long-lived stdin
    # pipe. Closing that pipe also cleans up the listener; no pkill is needed.
    try:
        while line := await asyncio.to_thread(sys.stdin.readline):
            if line.strip() == "stop":
                break
    finally:
        await bridge.close()
        emit("linux-stopped", **bridge.counts)


def validate_ready(row, expected_port):
    if row.get("event") != "linux-ready" or row.get("bindPort") != expected_port:
        raise ValueError("unexpected Linux peer handshake")
    private_ipv4(row["bindIP"])
    private_ipv4(row["allowedPeer"])
    if not isinstance(row.get("pid"), int) or row["pid"] <= 0:
        raise ValueError("invalid Linux peer PID")
    return row


async def wait_stop(path):
    while not path.exists():
        await asyncio.sleep(.25)


def write_new_json(path, value):
    with path.open("x", encoding="utf-8") as output:
        json.dump(value, output, indent=2)
        output.write("\n")


async def windows_peer(args):
    if os.name != "nt":
        raise ValueError("run this command from Windows, not from inside WSL")
    for path in [args.stop_file, args.ready_file, args.ready_file.with_suffix(".upstream.json")]:
        if not path.is_absolute() or path.exists() or not path.parent.is_dir():
            raise ValueError("state paths must be new absolute paths in an existing directory")
    if not args.linux_script.startswith("/") or "\n" in args.linux_script:
        raise ValueError("--linux-script must be an absolute WSL path to this script")
    child = None
    bridge = None
    watchers = []
    try:
        # Use an argv list, never a shell string. Distro/path spaces are safe.
        child = await asyncio.create_subprocess_exec(
            "wsl.exe", "-d", args.distro, "--exec", "python3", args.linux_script,
            "_linux", "--target-ip", args.target_ip, "--target-port", str(args.target_port),
            "--linux-port", str(args.linux_port), stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE)
        try:
            raw = await asyncio.wait_for(child.stdout.readline(), 30)
        except TimeoutError as exc:
            raise RuntimeError("WSL peer startup timed out; check WSL sessions and host memory before retrying") from exc
        ready = validate_ready(json.loads(raw), args.linux_port)
        write_new_json(args.ready_file.with_suffix(".upstream.json"), ready)
        bridge = TCPForwarder((ready["bindIP"], ready["bindPort"]))
        deadline = time.monotonic() + args.wait_listen
        while True:
            try:
                await bridge.start(("127.0.0.1", args.listen_port))
                break
            except OSError as exc:
                # Optional bounded handover waiting retries ONLY bind(), never
                # an HTTP request and never a connection to the application.
                # Windows exclusive binds may report WSAEACCES (10013) when
                # another listener owns the port, rather than WSAEADDRINUSE.
                if exc.errno not in (98, 10048, 10013) or time.monotonic() >= deadline or args.stop_file.exists():
                    raise
                await asyncio.sleep(.1)
        report = dict(event="windows-ready", pid=os.getpid(), wslPID=child.pid,
                      listenIP="127.0.0.1", listenPort=args.listen_port, upstream=ready)
        write_new_json(args.ready_file, report)
        emit("windows-ready", **{k: v for k, v in report.items() if k != "event"})
        watchers = [asyncio.create_task(wait_stop(args.stop_file)), asyncio.create_task(child.wait())]
        done, _ = await asyncio.wait(watchers, return_when=asyncio.FIRST_COMPLETED)
        if watchers[1] in done:
            raise RuntimeError("Linux peer exited; inspect its log and restart this helper")
    finally:
        if bridge:
            await bridge.close()
            emit("windows-stopped", **bridge.counts)
        if child is not None:
            if child.returncode is None:
                with contextlib.suppress(OSError):
                    child.stdin.write(b"stop\n")
                    await child.stdin.drain()
                child.stdin.close()
                try:
                    await asyncio.wait_for(child.wait(), 15)
                except TimeoutError:
                    # This is our one child process, never a distro-wide kill.
                    child.kill()
                    await child.wait()
            tail = await asyncio.wait_for(child.stdout.read(), 3)
            if tail:
                print(tail.decode("utf-8", errors="replace"), end="", flush=True)
        for task in watchers:
            task.cancel()
        await asyncio.gather(*watchers, return_exceptions=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["windows", "_linux"])
    parser.add_argument("--target-ip", type=private_ipv4, required=True)
    parser.add_argument("--target-port", type=port_number, required=True)
    parser.add_argument("--linux-port", type=port_number, required=True)
    parser.add_argument("--distro", default="Ubuntu-24.04")
    parser.add_argument("--linux-script")
    parser.add_argument("--listen-port", type=port_number)
    parser.add_argument("--stop-file", type=pathlib.Path)
    parser.add_argument("--ready-file", type=pathlib.Path)
    parser.add_argument("--wait-listen", type=float, default=0)
    args = parser.parse_args(argv)
    if args.mode == "windows" and (not args.linux_script or not args.listen_port or not args.stop_file or not args.ready_file):
        parser.error("windows mode requires --linux-script, --listen-port, --stop-file and --ready-file")
    if not 0 <= args.wait_listen <= 60:
        parser.error("--wait-listen must be between 0 and 60 seconds")
    try:
        asyncio.run(windows_peer(args) if args.mode == "windows" else linux_peer(args))
    except (OSError, ValueError, RuntimeError, TimeoutError) as exc:
        emit("failed", error=str(exc))
        return 1
    except KeyboardInterrupt:
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
