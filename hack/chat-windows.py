#!/usr/bin/env python3
"""Use Windows-local Ollama with sw's shared tool loop inside WSL.

No packages, model downloads, listeners, credentials or machine configuration
are installed. The launcher relays typed model frames; it never executes model
text. This one-shot entry supports reads/plans; human approval stays in sw's
interactive Linux CLI. For a multi-turn chat, run sw chat beside Linux Ollama.
"""
import argparse
import json
import re
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


def chat_command(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sw', required=True, help='absolute Linux sw path')
    parser.add_argument('--distro', default='Ubuntu-24.04')
    parser.add_argument('--model', required=True, help='installed Windows Ollama model name')
    parser.add_argument('--config-dir', required=True, help='user-owned Linux delegated config')
    parser.add_argument('--listen', default='127.0.0.1:6443')
    parser.add_argument('--prompt', required=True)
    parser.add_argument('--allow-write', action='store_true', help='allow creating plans; no automatic approval')
    parser.add_argument('--target', default='', help='one deploy/NAME for a focused session')
    parser.add_argument('-n', '--namespace', default=None, help='target namespace (default: default)')
    parser.add_argument('--task', choices=('inspect', 'image', 'scale'), default=None)
    parser.add_argument('--target-container', default=None)
    parser.add_argument('--target-uid', default=None)
    parser.add_argument('--target-generation', type=int, default=None)
    args = parser.parse_args(argv)
    if not args.sw.startswith('/') or not args.config_dir.startswith('/'):
        parser.error('sw and config-dir must be absolute Linux paths')
    command = ['wsl.exe', '-d', args.distro, '--exec', args.sw, 'chat', '--model-stdio',
               '--model=' + args.model, '--config-dir=' + args.config_dir,
               '--listen=' + args.listen, '--prompt=' + args.prompt]
    if args.allow_write:
        command.append('--allow-write')
    focus_values = (args.namespace, args.task, args.target_container, args.target_uid, args.target_generation)
    if not args.target:
        if any(value is not None for value in focus_values):
            parser.error('focus options require --target')
        return command
    # WSL을 띄우기 전에 집중 모드의 범위를 검사한다. 문자열을 셸 명령으로 조합하지 않는다.
    label = r'[a-z0-9](?:[a-z0-9-]*[a-z0-9])?'
    parts = args.target.split('/')
    if len(parts) != 2 or parts[0] not in ('deploy', 'deployment', 'deployments') or len(parts[1]) > 253 or not all(re.fullmatch(label, p) for p in parts[1].split('.')):
        parser.error('target must be one deploy/NAME')
    namespace = args.namespace if args.namespace is not None else 'default'
    if len(namespace) > 63 or not re.fullmatch(label, namespace):
        parser.error('invalid target namespace')
    task = args.task or 'inspect'
    if task != 'inspect' and not args.allow_write:
        parser.error('image and scale tasks require --allow-write')
    if args.target_container is not None and (task != 'image' or len(args.target_container) > 63 or not re.fullmatch(label, args.target_container)):
        parser.error('target-container requires an image task and a valid container name')
    if (args.target_uid is None) != (args.target_generation is None):
        parser.error('target-uid and target-generation must be supplied together')
    if args.target_uid is not None and (not 1 <= len(args.target_uid.encode('utf-8')) <= 256 or not 1 <= args.target_generation <= 9007199254740991):
        parser.error('target goal requires a nonempty UID and a positive safe generation')
    command.extend(['--target=' + args.target, '-n=' + namespace, '--task=' + task])
    for name in ('target_container', 'target_uid', 'target_generation'):
        value = getattr(args, name)
        if value is not None:
            command.append('--' + name.replace('_', '-') + '=' + str(value))
    return command


def main():
    command = chat_command()
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
