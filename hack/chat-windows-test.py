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
