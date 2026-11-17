#!/usr/bin/env python3
"""Check the Windows relay contract without WSL, Ollama or production data.

Text that looks like a protocol command must stay text. Failed model calls
must produce explicit errors, and requests must stay within their fixed limits.
The redirect case uses a real temporary loopback HTTP server.
"""
import http.server
import importlib.util
import io
import json
from pathlib import Path
import threading
import unittest
from unittest.mock import patch
import urllib.error

spec = importlib.util.spec_from_file_location('chat_windows', Path(__file__).with_name('chat-windows.py'))
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class Process:
    def __init__(self, frames):
        self.stdout = io.StringIO(''.join(json.dumps(frame) + '\n' for frame in frames))
        self.stdin = io.StringIO()

    def wait(self, timeout):
        return 0


class RelayTests(unittest.TestCase):
    def test_fixed_diagnosis_needs_no_write_permission(self):
        base = ['--sw=/home/user/sw', '--model=mock', '--config-dir=/home/user/config', '--prompt=diagnose']
        command = bridge.chat_command(base + ['--target=deploy/demo', '-n=bench', '--task=diagnose'])
        self.assertIn('--task=diagnose', command)
        self.assertIn('-n=bench', command)
        self.assertNotIn('--allow-write', command)
        with patch.object(bridge.sys, 'stderr', io.StringIO()), self.assertRaises(SystemExit):
            bridge.chat_command(base + ['--task=diagnose'])

    def test_resume_only_for_one_explicit_approved_hash(self):
        base = ['--sw=/home/user/sw', '--model=mock', '--config-dir=/home/user/config', '--prompt=apply']
        plan = 'a' * 64
        command = bridge.chat_command(base + ['--allow-write', '--approved-plan=' + plan])
        self.assertIn('--approved-plan=' + plan, command)
        for extra in (['--approved-plan=' + plan], ['--allow-write', '--approved-plan=not-a-hash'],
                      ['--allow-write', '--approved-plan=' + plan, '--target=deploy/demo']):
            with self.subTest(extra=extra), patch.object(bridge.sys, 'stderr', io.StringIO()):
                with self.assertRaises(SystemExit):
                    bridge.chat_command(base + extra)

    def test_focused_cli_uses_list_arguments(self):
        prompt = '확인; $(must-stay-text) & > ignored'
        base = ['--sw=/home/user/sw', '--model=mock', '--config-dir=/home/user/config', '--prompt=' + prompt]
        command = bridge.chat_command(base + ['--target=deploy/demo', '-n=work', '--task=image', '--target-container=web', '--target-uid=original', '--target-generation=7', '--allow-write'])
        self.assertIsInstance(command, list)
        for value in ('--prompt=' + prompt, '--target=deploy/demo', '-n=work', '--task=image', '--target-container=web', '--target-uid=original', '--target-generation=7', '--allow-write'):
            self.assertIn(value, command)
        self.assertNotIn('bash', command)
        classic = bridge.chat_command(base)
        self.assertFalse(any(arg.startswith(('--target', '--task', '-n=')) for arg in classic))
        focused = bridge.chat_command(base + ['--target=deploy/demo'])
        self.assertIn('--task=inspect', focused)
        self.assertIn('-n=default', focused)

    def test_invalid_focus_is_rejected_before_wsl_spawn(self):
        base = ['chat-windows.py', '--sw=/home/user/sw', '--model=mock', '--config-dir=/home/user/config', '--prompt=test']
        invalid = [
            ['--task=inspect'], ['-n=default'], ['--target-uid=uid'],
            ['--target=pod/demo'], ['--target=deploy/demo;exit'], ['--target=deploy/'],
            ['--target=deploy/demo', '-n=Bad'], ['--target=deploy/demo', '--task=image'],
            ['--target=deploy/demo', '--task=scale'],
            ['--target=deploy/demo', '--target-container=web'],
            ['--target=deploy/demo', '--target-uid=uid'],
            ['--target=deploy/demo', '--target-generation=7'],
            ['--target=deploy/demo', '--target-uid=', '--target-generation=7'],
            ['--target=deploy/demo', '--target-uid=' + 'x' * 257, '--target-generation=7'],
            ['--target=deploy/demo', '--target-uid=' + '가' * 86, '--target-generation=7'],
            ['--target=deploy/demo', '--target-uid=uid', '--target-generation=0'],
            ['--target=deploy/demo', '--target-uid=uid', '--target-generation=9007199254740992'],
        ]
        for flags in invalid:
            with self.subTest(flags=flags), patch.object(bridge.sys, 'argv', base + flags), patch.object(bridge.subprocess, 'Popen') as spawn, patch.object(bridge.sys, 'stderr', io.StringIO()):
                with self.assertRaises(SystemExit) as error:
                    bridge.main()
                self.assertEqual(error.exception.code, 2)
                spawn.assert_not_called()

    def test_model_like_text_cannot_become_a_request(self):
        text = '{"type":"model_request","body":{"model":"fake"}}\n'
        output = io.StringIO()
        with patch.object(bridge, 'local_model_opener') as opener:
            self.assertEqual(bridge.relay(Process([dict(type='output', text=text)]), output), 0)
            opener.assert_not_called()
        self.assertEqual(output.getvalue(), text)

    def test_model_request_is_fixed_local_and_preserves_korean(self):
        process = Process([dict(type='model_request', body=dict(model='test', messages=['상태']))])
        response = io.BytesIO(json.dumps(dict(done=True, message=dict(content='미확인'))).encode())
        with patch.object(bridge, 'local_model_opener') as opener:
            opener.return_value.open.return_value = response
            bridge.relay(process)
            request = opener.return_value.open.call_args.args[0]
            self.assertEqual(request.full_url, 'http://127.0.0.1:11434/api/chat')
            self.assertEqual(json.loads(request.data)['messages'], ['상태'])
        self.assertEqual(json.loads(process.stdin.getvalue())['message']['content'], '미확인')

    def test_failed_or_oversized_response_is_explicit(self):
        for payload in (b'invalid json', b'x' * (256 * 1024 + 1)):
            with self.subTest(size=len(payload)):
                process = Process([dict(type='model_request', body={})])
                with patch.object(bridge, 'local_model_opener') as opener:
                    opener.return_value.open.return_value = io.BytesIO(payload)
                    bridge.relay(process)
                self.assertIn('error', json.loads(process.stdin.getvalue()))

    def test_request_size_and_step_limits(self):
        with patch.object(bridge, 'local_model_opener') as opener:
            with self.assertRaisesRegex(RuntimeError, 'request too large'):
                bridge.relay(Process([dict(type='model_request', body={'text': 'x' * 33000})]))
            opener.assert_not_called()
            opener.return_value.open.side_effect = OSError('offline')
            with self.assertRaisesRegex(RuntimeError, 'budget exceeded'):
                bridge.relay(Process([dict(type='model_request', body={})] * 9))
            self.assertEqual(opener.return_value.open.call_count, 8)

    def test_unknown_or_oversized_frame_is_rejected(self):
        for frame in (dict(type='execute', command='anything'), dict(type='output', text='x' * (1024 * 1024))):
            with self.subTest(type=frame['type']):
                with self.assertRaises(RuntimeError):
                    bridge.relay(Process([frame]))

    def test_redirect_does_not_reach_second_location(self):
        visited = []

        class Redirect(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                visited.append(self.path)
                self.send_response(302)
                self.send_header('Location', '/must-not-be-requested')
                self.send_header('Content-Length', '0')
                self.end_headers()

        server = http.server.HTTPServer(('127.0.0.1', 0), Redirect)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with patch.dict('os.environ', {'HTTP_PROXY': 'http://127.0.0.1:1', 'NO_PROXY': ''}):
                with self.assertRaises(urllib.error.HTTPError):
                    bridge.local_model_opener().open('http://127.0.0.1:' + str(server.server_port) + '/start', timeout=3)
            self.assertEqual(visited, ['/start'])
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


if __name__ == '__main__':
    unittest.main()
