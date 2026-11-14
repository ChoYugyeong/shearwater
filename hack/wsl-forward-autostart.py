#!/usr/bin/env python3
"""Keep the optional Windows TCP bridge alive across WSL distribution restarts.

Windows login starts this supervisor once. It keeps one child WSL session,
detects that session ending, waits with bounded backoff, and obtains the new
WSL IPv4 address when reconnecting. It never restarts an app, changes its data,
or replays an HTTP request. Linux systemd separately starts the existing sw.
"""
import argparse
import asyncio
import contextlib
import importlib.util
import json
import logging
import logging.handlers
import os
import pathlib
import socket
import sys
import time
import uuid
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location("wsl_forward", pathlib.Path(__file__).with_name("wsl-forward.py"))
forward = importlib.util.module_from_spec(spec)
spec.loader.exec_module(forward)


class AlreadyRunning(RuntimeError):
    pass


class InstanceLock:
    """An OS-held lock, not a PID-file guess; a crash releases it automatically.

    Keep the lock file itself. Removing a locked file can allow two processes
    to lock two different files bearing the same name. The first byte is the
    lock range on Windows; POSIX locking is used only by portable tests.
    """
    def __init__(self, path):
        self.file = path.open("a+b")
        if self.file.tell() == 0:
            self.file.write(b"\0")
            self.file.flush()
        self.file.seek(0)
        try:
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(self.file.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(self.file.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            self.file.close()
            raise AlreadyRunning("another supervisor owns this state directory") from exc

    def close(self):
        self.file.close()

    def __enter__(self):
        return self

    def __exit__(self, *unused):
        self.close()


def validate_config(config):
    expected = {"schema", "distro", "target_ip", "target_port", "linux_port", "listen_port", "linux_script"}
    if set(config) != expected or type(config["schema"]) is not int or config["schema"] != 1:
        raise ValueError("configuration must have schema=1 and exactly the documented keys")
    if not isinstance(config["distro"], str) or not config["distro"] or any(ord(c) < 32 for c in config["distro"]):
        raise ValueError("invalid WSL distribution name")
    forward.private_ipv4(config["target_ip"])
    for key in ("target_port", "linux_port", "listen_port"):
        if type(config[key]) is not int:
            raise ValueError(key + " must be an integer")
        if not 1 <= config[key] <= 65535:
            raise ValueError(key + " must be between 1 and 65535")
    script = config["linux_script"]
    if not isinstance(script, str) or not script.startswith("/") or any(ord(c) < 32 for c in script):
        raise ValueError("linux_script must be an absolute WSL path")
    return config


def retry_seconds(failures):
    # Avoid repeatedly launching WSL when boot/network/memory is unavailable.
    return min(30, 2 ** min(max(failures, 1), 5))


def atomic_json(path, value):
    # Readers see either the old complete document or the new one. Only these
    # two files inside the owned state directory are replaced; no glob cleanup.
    temporary = path.with_suffix(".pending")
    with temporary.open("w", encoding="utf-8") as output:
        json.dump(value, output, indent=2)
        output.write("\n")
        output.flush()
    os.replace(temporary, path)


def make_logger(state_dir):
    logger = logging.getLogger("shearwater-forward-" + str(state_dir))
    logger.setLevel(logging.INFO)
    logger.propagate = False
    handler = logging.handlers.RotatingFileHandler(
        state_dir / "supervisor.log", maxBytes=1024 * 1024, backupCount=3, encoding="utf-8")
    handler.setFormatter(logging.Formatter("%(message)s"))
    logger.addHandler(handler)
    return logger, handler


def probe_listener(port):
    """Refuse a foreign listener before starting a WSL session. Never kill it."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        if os.name == "nt":
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
        sock.bind(("127.0.0.1", port))


async def supervise(config, state_dir, *, bridge=None, check_listener=probe_listener, sleep=None):
    validate_config(config)
    bridge = bridge or forward.windows_peer
    if not state_dir.is_absolute() or not state_dir.is_dir():
        raise ValueError("state directory must already exist and be an absolute path")
    with InstanceLock(state_dir / "instance.lock"):
        logger, handler = make_logger(state_dir)
        instance = uuid.uuid4().hex
        stop = state_dir / ("stop-" + instance)
        state = dict(schema=1, instance=instance, pid=os.getpid(), phase="starting",
                     attempt=0, failures=0, updatedAtUnix=time.time(), lastError=None,
                     config=config, stopFile=str(stop), upstream=None)

        def publish(phase, **values):
            state.update(phase=phase, updatedAtUnix=time.time(), **values)
            atomic_json(state_dir / "status.json", state)

        def event(name, **values):
            # The Linux handshake already includes its own wall timestamp.
            # Keep it separate from Windows observation time, without duplicate
            # keyword arguments or an ambiguous cross-machine clock value.
            record = dict(values)
            if "unix" in record:
                record["sourceUnix"] = record.pop("unix")
            record.update(event=name, unix=time.time())
            logger.info(json.dumps(record))
            if name == "windows-ready":
                publish("running", upstream=values["upstream"], wslPID=values["wslPID"], lastError=None)

        async def pause(seconds):
            if sleep:
                await sleep(seconds)
            else:
                with contextlib.suppress(TimeoutError):
                    await asyncio.wait_for(forward.wait_stop(stop), seconds)

        try:
            publish("starting")
            event("supervisor-started", instance=instance, pid=os.getpid())
            while not stop.exists():
                state["attempt"] += 1
                started = time.monotonic()
                publish("starting", upstream=None)
                try:
                    check_listener(config["listen_port"])
                    args = SimpleNamespace(**{k: v for k, v in config.items() if k != "schema"},
                                           ready_file=None, stop_file=stop, wait_listen=0)
                    await bridge(args, on_event=event)
                    if stop.exists():
                        break
                    raise RuntimeError("bridge ended without a stop request")
                except Exception as exc:
                    # Each failed attempt has finished cleanup before the next
                    # one begins. HTTP bytes are never queued across attempts.
                    if stop.exists():
                        break
                    state["failures"] = 1 if time.monotonic() - started >= 60 else state["failures"] + 1
                    delay = retry_seconds(state["failures"])
                    message = type(exc).__name__ + ": " + str(exc)
                    publish("backoff", lastError=message, upstream=None, retrySeconds=delay)
                    event("retry", error=message, seconds=delay, attempt=state["attempt"])
                    await pause(delay)
        finally:
            publish("stopped", upstream=None)
            event("supervisor-stopped", instance=instance)
            logger.removeHandler(handler)
            handler.close()


def status(state_dir):
    file = state_dir / "status.json"
    if not file.exists():
        return dict(phase="not-started", active=False)
    value = json.loads(file.read_text(encoding="utf-8"))
    try:
        with InstanceLock(state_dir / "instance.lock"):
            value["active"] = False
    except AlreadyRunning:
        value["active"] = True
    return value


def request_stop(state_dir):
    value = status(state_dir)
    if not value["active"]:
        return value
    # Derive the path from a validated random ID. Never trust a status file's
    # arbitrary stopFile path, and never signal a possibly reused PID.
    instance = value["instance"]
    if uuid.UUID(instance).hex != instance:
        raise ValueError("invalid supervisor instance ID")
    stop = state_dir / ("stop-" + instance)
    with contextlib.suppress(FileExistsError):
        with stop.open("x", encoding="utf-8") as output:
            output.write("operator requested stop\n")
    return dict(requested=True, instance=instance)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["run", "status", "stop"])
    parser.add_argument("--state-dir", required=True, type=pathlib.Path)
    parser.add_argument("--config", type=pathlib.Path)
    args = parser.parse_args(argv)
    if not args.state_dir.is_absolute():
        parser.error("--state-dir must be absolute")
    try:
        if args.action != "run":
            print(json.dumps(status(args.state_dir) if args.action == "status" else request_stop(args.state_dir), indent=2))
            return 0
        if os.name != "nt":
            raise ValueError("run the supervisor on Windows")
        if args.config is None or not args.config.is_absolute():
            parser.error("run requires an absolute --config path")
        config = validate_config(json.loads(args.config.read_text(encoding="utf-8")))
        asyncio.run(supervise(config, args.state_dir))
    except AlreadyRunning:
        # Login triggers and a manual Start-ScheduledTask may overlap. The
        # existing owner continues; a duplicate is a successful no-op.
        return 0
    except (OSError, ValueError, RuntimeError) as exc:
        if sys.stderr is not None:
            print(str(exc), file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
