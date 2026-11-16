#!/usr/bin/env python3
"""Use Windows-local Ollama with sw's shared tool loop inside WSL.

No packages, model downloads, listeners, credentials or machine configuration
are installed. The launcher relays typed model frames; it never executes model
text. This one-shot entry supports reads/plans; human approval stays in sw's
interactive Linux CLI. For a multi-turn chat, run sw chat beside Linux Ollama.
"""
import argparse
import json
import subprocess
import sys
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    # Even a loopback server can return a redirect. Keep question/tool data at
    # the chosen local model endpoint instead of following it to another host.
    def redirect_request(self, request, file, code, message, headers, newurl):
        return None


def local_model_opener():
    # Environment proxies and HTTP redirects must not widen this bridge's scope.
    return urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def relay(process, output=sys.stdout):
    requests = 0
    while True:
        line = process.stdout.readline(1024 * 1024 + 1)
        if not line:
            break
        if len(line) > 1024 * 1024:
            raise RuntimeError('bridge frame too large')
        frame = json.loads(line)
        if frame.get('type') == 'output':
            output.write(frame['text'])
            output.flush()
        elif frame.get('type') == 'model_request':
            requests += 1
            if requests > 8:
                raise RuntimeError('model request budget exceeded')
            body = json.dumps(frame['body'], ensure_ascii=False).encode('utf-8')
            if len(body) > 32 * 1024:
                raise RuntimeError('model request too large')
            request = urllib.request.Request('http://127.0.0.1:11434/api/chat', data=body,
                                             headers={'Content-Type': 'application/json'})
            opener = local_model_opener()
            try:
                with opener.open(request, timeout=90) as response:
                    raw = response.read(256 * 1024 + 1)
                if len(raw) > 256 * 1024:
                    raise RuntimeError('model response too large')
                result = json.loads(raw)
            except Exception:
                result = {'error': 'Windows local Ollama request failed'}
            process.stdin.write(json.dumps(result, ensure_ascii=False) + '\n')
            process.stdin.flush()
        else:
            raise RuntimeError('unknown bridge frame')
    return process.wait(timeout=10)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sw', required=True, help='absolute Linux sw path')
    parser.add_argument('--distro', default='Ubuntu-24.04')
    parser.add_argument('--model', required=True, help='installed Windows Ollama model name')
    parser.add_argument('--config-dir', required=True, help='user-owned Linux delegated config')
    parser.add_argument('--listen', default='127.0.0.1:6443')
    parser.add_argument('--prompt', required=True)
    parser.add_argument('--allow-write', action='store_true', help='allow creating plans; no automatic approval')
    args = parser.parse_args()
    if not args.sw.startswith('/') or not args.config_dir.startswith('/'):
        parser.error('sw and config-dir must be absolute Linux paths')
    command = ['wsl.exe', '-d', args.distro, '--exec', args.sw, 'chat', '--model-stdio',
               '--model=' + args.model, '--config-dir=' + args.config_dir,
               '--listen=' + args.listen, '--prompt=' + args.prompt]
    if args.allow_write:
        command.append('--allow-write')
    process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               text=True, encoding='utf-8')
    try:
        return relay(process)
    finally:
        if process.poll() is None:
            process.terminate()
            process.wait(timeout=10)


if __name__ == '__main__':
    sys.exit(main())
