"""Real-socket tests for the optional bridge, without WSL or root privileges.

The stalled-hop test deliberately reproduces a transport stall. It does not
exhaust RAM or claim to reproduce the Windows kernel's allocator failure.
"""
import asyncio
import argparse
import importlib.util
import pathlib
import socket
import unittest

spec = importlib.util.spec_from_file_location("wsl_forward", pathlib.Path(__file__).with_name("wsl-forward.py"))
forward = importlib.util.module_from_spec(spec)
spec.loader.exec_module(forward)


class ForwardTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.servers = []
        self.bridges = []
        self.writers = []
        self.handlers = set()

    async def asyncTearDown(self):
        for writer in self.writers:
            await forward.TCPForwarder.close_writer(writer)
        for bridge in self.bridges:
            await bridge.close(grace=.05)
        for server in self.servers:
            server.close()
        for task in list(self.handlers):
            task.cancel()
        await asyncio.gather(*self.handlers, return_exceptions=True)
        for server in self.servers:
            await server.wait_closed()

    async def server(self, handler):
        async def owned(reader, writer):
            task = asyncio.current_task()
            self.handlers.add(task)
            try:
                await handler(reader, writer)
            finally:
                await forward.TCPForwarder.close_writer(writer)
                self.handlers.discard(task)
        server = await asyncio.start_server(owned, "127.0.0.1", 0)
        self.servers.append(server)
        return server.sockets[0].getsockname()

    async def bridge(self, target, **kwargs):
        instance = forward.TCPForwarder(target, **kwargs)
        self.bridges.append(instance)
        return instance, await instance.start(("127.0.0.1", 0))

    async def client(self, address):
        reader, writer = await asyncio.open_connection(*address)
        self.writers.append(writer)
        return reader, writer

    async def test_full_duplex_large_payload(self):
        payload = bytes(range(256)) * 8192
        async def echo(reader, writer):
            while chunk := await reader.read(32768):
                writer.write(chunk)
                await writer.drain()
        target = await self.server(echo)
        bridge, address = await self.bridge(target)
        reader, writer = await self.client(address)
        async def upload():
            for start in range(0, len(payload), 4096):
                writer.write(payload[start:start + 4096])
                await writer.drain()
            writer.write_eof()
        sender = asyncio.create_task(upload())
        received = await asyncio.wait_for(reader.read(), 10)
        await sender
        self.assertEqual(payload, received)
        self.assertEqual(bridge.counts["errors"], 0)
        self.assertEqual(bridge.counts["bytesUp"], len(payload))
        self.assertEqual(bridge.counts["bytesDown"], len(payload))

    async def test_client_half_close_preserves_delayed_response(self):
        async def respond(reader, writer):
            request = await reader.read()
            await asyncio.sleep(.05)
            writer.write(b"received:" + request)
            await writer.drain()
        target = await self.server(respond)
        _, address = await self.bridge(target)
        reader, writer = await self.client(address)
        writer.write(b"upload")
        writer.write_eof()
        self.assertEqual(await asyncio.wait_for(reader.read(), 2), b"received:upload")

    async def test_backend_half_close_still_accepts_upload(self):
        upload = asyncio.get_running_loop().create_future()
        async def serve(reader, writer):
            writer.write(b"hello")
            writer.write_eof()
            upload.set_result(await reader.read())
        target = await self.server(serve)
        _, address = await self.bridge(target)
        reader, writer = await self.client(address)
        self.assertEqual(await asyncio.wait_for(reader.read(), 2), b"hello")
        writer.write(b"after-fin")
        writer.write_eof()
        self.assertEqual(await asyncio.wait_for(upload, 2), b"after-fin")

    async def test_unreachable_target_is_error_and_never_retried(self):
        # Reserve a bound but non-listening port so another test cannot use it.
        with socket.socket() as reserved:
            reserved.bind(("127.0.0.1", 0))
            bridge, address = await self.bridge(reserved.getsockname(), connect_timeout=.1)
            reader, _ = await self.client(address)
            self.assertEqual(await asyncio.wait_for(reader.read(), 2), b"")
        self.assertEqual(bridge.counts["accepted"], 1)
        self.assertEqual(bridge.counts["errors"], 1)
        self.assertEqual(bridge.counts["retries"], 0)

    async def test_other_peer_rejected_before_target_connect(self):
        upstream_calls = []
        async def serve(reader, writer):
            upstream_calls.append(True)
        target = await self.server(serve)
        bridge, address = await self.bridge(target, allowed_peer="192.0.2.1")
        reader, _ = await self.client(address)
        self.assertEqual(await asyncio.wait_for(reader.read(), 2), b"")
        self.assertEqual(upstream_calls, [])
        self.assertEqual(bridge.counts["rejected"], 1)

    async def test_partial_post_response_does_not_replay_write(self):
        # The server may have committed a write before its response breaks.
        # Count actual received writes, not the bridge's own retry counter.
        received = []
        async def commit_then_break(reader, writer):
            await reader.readuntil(b"\r\n\r\n")
            received.append(await reader.readexactly(4))
            writer.write(b"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
            await writer.drain()
            # The handler closes before the declared body length is sent.
        target = await self.server(commit_then_break)
        _, address = await self.bridge(target)
        reader, writer = await self.client(address)
        writer.write(b"POST /write HTTP/1.1\r\nHost: local\r\nContent-Length: 4\r\n\r\nonce")
        writer.write_eof()
        response = await asyncio.wait_for(reader.read(), 2)
        self.assertTrue(response.endswith(b"\r\n\r\npartial"))
        await asyncio.sleep(.05)
        self.assertEqual(received, [b"once"])

    async def test_limit_rejects_excess_connections(self):
        async def wait(reader, writer):
            await reader.read()
        target = await self.server(wait)
        bridge, address = await self.bridge(target, max_connections=1)
        await self.client(address)
        await asyncio.sleep(.02)
        reader, _ = await self.client(address)
        self.assertEqual(await asyncio.wait_for(reader.read(), 2), b"")
        self.assertEqual(bridge.counts["peakActive"], 1)
        self.assertEqual(bridge.counts["rejected"], 1)

    async def test_shutdown_drains_inflight_response(self):
        started = asyncio.Event()
        release = asyncio.Event()
        async def serve(reader, writer):
            await reader.readexactly(1)
            started.set()
            await release.wait()
            writer.write(b"complete")
            await writer.drain()
        target = await self.server(serve)
        bridge, address = await self.bridge(target)
        reader, writer = await self.client(address)
        writer.write(b"x")
        writer.write_eof()
        await started.wait()
        closing = asyncio.create_task(bridge.close(grace=2))
        await asyncio.sleep(.02)
        self.assertFalse(closing.done())
        release.set()
        self.assertEqual(await reader.read(), b"complete")
        await closing
        self.assertEqual(bridge.counts["cancelled"], 0)

    async def test_shutdown_cancels_stuck_peer_within_grace(self):
        async def wait(reader, writer):
            await reader.read()
        target = await self.server(wait)
        bridge, address = await self.bridge(target)
        reader, _ = await self.client(address)
        await asyncio.sleep(.02)
        await asyncio.wait_for(bridge.close(grace=.02), 2)
        self.assertEqual(await reader.read(), b"")
        self.assertEqual(bridge.tasks, set())
        self.assertEqual(bridge.counts["cancelled"], 1)

    async def test_port_collision_does_not_displace_listener(self):
        async def hello(reader, writer):
            writer.write(b"original")
        address = await self.server(hello)
        bridge = forward.TCPForwarder(address)
        with self.assertRaises(OSError):
            await bridge.start(address)
        reader, _ = await self.client(address)
        self.assertEqual(await reader.read(), b"original")

    async def test_five_stalled_hop_requests_and_direct_path(self):
        # Fault injection: this intermediate hop accepts TCP but never passes
        # application bytes. This tests bypass behavior, not host RAM pressure.
        async def stalled_hop(reader, writer):
            await asyncio.Event().wait()
        async def healthy_app(reader, writer):
            await reader.readuntil(b"\r\n\r\n")
            writer.write(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK")
        stalled = await self.server(stalled_hop)
        app = await self.server(healthy_app)
        _, old_address = await self.bridge(stalled)
        fixed, new_address = await self.bridge(app)
        timeouts = 0
        for _ in range(5):
            reader, writer = await self.client(old_address)
            writer.write(b"GET / HTTP/1.1\r\nHost: local\r\n\r\n")
            with self.assertRaises(TimeoutError):
                await asyncio.wait_for(reader.readexactly(1), .05)
            timeouts += 1
            await forward.TCPForwarder.close_writer(writer)
            reader, writer = await self.client(new_address)
            writer.write(b"GET / HTTP/1.1\r\nHost: local\r\n\r\n")
            writer.write_eof()
            self.assertTrue((await asyncio.wait_for(reader.read(), 2)).endswith(b"\r\n\r\nOK"))
        self.assertEqual(timeouts, 5)
        self.assertEqual(fixed.counts["errors"], 0)
        self.assertEqual(fixed.counts["retries"], 0)


class ValidationTests(unittest.TestCase):
    def test_invalid_peer_handshakes_are_refused(self):
        good = dict(event="linux-ready", pid=123, bindIP="172.23.1.2", bindPort=18081, allowedPeer="172.23.0.1")
        self.assertEqual(forward.validate_ready(good, 18081), good)
        for override in [dict(event="other"), dict(bindPort=80), dict(bindIP="0.0.0.0"),
                         dict(bindIP="127.0.0.1"), dict(allowedPeer="8.8.8.8"), dict(pid=-1)]:
            with self.subTest(override=override), self.assertRaises(ValueError):
                forward.validate_ready(dict(good, **override), 18081)

    def test_port_boundaries(self):
        for value in (0, 65536, -1):
            with self.assertRaises(argparse.ArgumentTypeError):
                forward.port_number(value)
        self.assertEqual(forward.port_number("65535"), 65535)


if __name__ == "__main__":
    unittest.main()
