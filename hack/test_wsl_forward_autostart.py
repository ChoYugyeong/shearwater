"""Supervisor lifecycle tests: no host restart, root, or real app changes."""
import asyncio
import importlib.util
import json
import pathlib
import socket
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("forward_auto", pathlib.Path(__file__).with_name("wsl-forward-autostart.py"))
auto = importlib.util.module_from_spec(spec)
spec.loader.exec_module(auto)

CONFIG = dict(schema=1, distro="Test Distro", target_ip="10.96.0.1", target_port=80,
              linux_port=18081, listen_port=18080, linux_script="/opt/shearwater/wsl-forward.py")


class SupervisorTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="sw-forward-test-")
        self.root = pathlib.Path(self.temp.name)

    async def asyncTearDown(self):
        self.temp.cleanup()

    def test_second_owner_refused_and_close_releases_lock(self):
        lock = self.root / "instance.lock"
        with auto.InstanceLock(lock):
            with self.assertRaises(auto.AlreadyRunning):
                auto.InstanceLock(lock)
        # A stale file alone is not a stale owner. Reopening is safe.
        with auto.InstanceLock(lock):
            self.assertTrue(lock.exists())

    async def test_wsl_exit_reconnects_with_new_ip_then_stops(self):
        attempts = []
        delays = []
        async def bridge(args, on_event):
            attempts.append(args)
            on_event("linux-ready", unix=123.5, bindIP="172.23.1." + str(len(attempts)))
            on_event("windows-ready", wslPID=len(attempts), upstream={"bindIP": "172.23.1." + str(len(attempts))})
            running = auto.status(self.root)
            self.assertEqual(running["phase"], "running")
            self.assertTrue(running["active"])
            if len(attempts) == 1:
                raise RuntimeError("WSL distribution terminated")
            self.assertTrue(auto.request_stop(self.root)["requested"])
        async def sleep(delay):
            delays.append(delay)
        await auto.supervise(CONFIG, self.root, bridge=bridge, sleep=sleep, check_listener=lambda _: None)
        self.assertEqual(len(attempts), 2)
        self.assertEqual(delays, [2])
        self.assertEqual(attempts[0].stop_file, attempts[1].stop_file)
        self.assertEqual(auto.status(self.root)["phase"], "stopped")
        self.assertFalse(auto.status(self.root)["active"])
        events = [json.loads(line) for line in (self.root / "supervisor.log").read_text().splitlines()]
        self.assertEqual([e["upstream"]["bindIP"] for e in events if e["event"] == "windows-ready"], ["172.23.1.1", "172.23.1.2"])
        self.assertTrue(all(e["sourceUnix"] == 123.5 and e["unix"] > 123.5 for e in events if e["event"] == "linux-ready"))

    async def test_repeated_startup_failure_is_serial_and_backed_off(self):
        delays = []
        running = 0
        async def bridge(args, on_event):
            nonlocal running
            running += 1
            self.assertEqual(running, 1)
            await asyncio.sleep(0)
            running -= 1
            raise OSError("WSL is temporarily unavailable")
        async def sleep(delay):
            delays.append(delay)
            if len(delays) == 6:
                auto.request_stop(self.root)
        await auto.supervise(CONFIG, self.root, bridge=bridge, sleep=sleep, check_listener=lambda _: None)
        self.assertEqual(delays, [2, 4, 8, 16, 30, 30])
        self.assertEqual(auto.status(self.root)["attempt"], 6)

    async def test_foreign_listener_preserved_without_starting_wsl(self):
        calls = []
        async def bridge(args, on_event):
            calls.append(True)
        async def sleep(delay):
            auto.request_stop(self.root)
        with socket.socket() as foreign:
            foreign.bind(("127.0.0.1", 0))
            foreign.listen()
            config = dict(CONFIG, listen_port=foreign.getsockname()[1])
            await auto.supervise(config, self.root, bridge=bridge, sleep=sleep)
            with socket.create_connection(foreign.getsockname(), timeout=1):
                accepted, _ = foreign.accept()
                accepted.close()
        self.assertEqual(calls, [])
        self.assertEqual(auto.status(self.root)["failures"], 1)

    async def test_stop_interrupts_real_backoff(self):
        failed = asyncio.Event()
        async def bridge(args, on_event):
            failed.set()
            raise OSError("offline")
        task = asyncio.create_task(auto.supervise(CONFIG, self.root, bridge=bridge, check_listener=lambda _: None))
        await failed.wait()
        self.assertEqual(auto.status(self.root)["phase"], "backoff")
        auto.request_stop(self.root)
        await asyncio.wait_for(task, 1)
        self.assertEqual(auto.status(self.root)["attempt"], 1)

    async def test_old_stop_request_does_not_stop_next_instance(self):
        instances = []
        async def bridge(args, on_event):
            instances.append(auto.status(self.root)["instance"])
            auto.request_stop(self.root)
        for _ in range(2):
            await auto.supervise(CONFIG, self.root, bridge=bridge, check_listener=lambda _: None)
        self.assertEqual(len(set(instances)), 2)

    async def test_duplicate_supervisor_never_spawns_a_child(self):
        called = []
        async def bridge(args, on_event):
            called.append(True)
        with auto.InstanceLock(self.root / "instance.lock"):
            with self.assertRaises(auto.AlreadyRunning):
                await auto.supervise(CONFIG, self.root, bridge=bridge, check_listener=lambda _: None)
        self.assertEqual(called, [])

    def test_invalid_config_is_rejected_before_any_child(self):
        for values in [dict(schema=2), dict(target_ip="8.8.8.8"), dict(target_port="80"),
                       dict(linux_script="relative.py"), dict(distro="bad\nname"), dict(extra=True)]:
            with self.subTest(values=values), self.assertRaises(ValueError):
                auto.validate_config(dict(CONFIG, **values))

    def test_reading_status_before_start_does_not_create_state(self):
        empty = self.root / "absent"
        self.assertEqual(auto.status(empty), dict(phase="not-started", active=False))
        self.assertFalse(empty.exists())

    def test_tampered_stop_path_does_not_write_outside_owned_directory(self):
        value = dict(instance="../outside", phase="running", stopFile=str(self.root.parent / "outside"))
        (self.root / "status.json").write_text(json.dumps(value))
        with auto.InstanceLock(self.root / "instance.lock"):
            with self.assertRaises(ValueError):
                auto.request_stop(self.root)


if __name__ == "__main__":
    unittest.main()
