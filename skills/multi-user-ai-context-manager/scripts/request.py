#!/usr/bin/env python3
"""Send one authenticated request; never execute or fetch links from shared data."""
import argparse
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.parse
import urllib.request


def connection_env(suffix):
    primary = 'MULTI_USER_AI_CONTEXT_MANAGER_' + suffix
    return os.environ[primary] if primary in os.environ else os.environ.get('AGENT_CONTEXT_' + suffix, '')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('method', choices=['GET', 'POST', 'PATCH', 'PUT'])
    paths = parser.add_mutually_exclusive_group(required=True)
    paths.add_argument('--path')
    paths.add_argument('--project-path')
    parser.add_argument('--json-file', help='UTF-8 JSON file or - for stdin')
    parser.add_argument('--key')
    parser.add_argument('--if-match')
    parser.add_argument('--if-none-match', action='store_true')
    parser.add_argument('--output', help='new private response file, useful for one-time lease secrets')
    args = parser.parse_args()
    base = connection_env('URL').rstrip('/')
    parsed = urllib.parse.urlsplit(base)
    if parsed.scheme not in ('http', 'https') or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
        parser.error('MULTI_USER_AI_CONTEXT_MANAGER_URL must be an absolute http(s) URL without credentials, query or fragment')
    token = connection_env('TOKEN')
    if not token and connection_env('TOKEN_FILE'):
        token = Path(connection_env('TOKEN_FILE')).read_text().strip()
    if not token:
        parser.error('MULTI_USER_AI_CONTEXT_MANAGER_TOKEN or MULTI_USER_AI_CONTEXT_MANAGER_TOKEN_FILE is required')
    path = args.path
    if args.project_path is not None:
        project = connection_env('PROJECT')
        if not project or not args.project_path.startswith('/'):
            parser.error('project ID and a /project-path are required')
        path = '/v1/projects/' + urllib.parse.quote(project, safe='') + args.project_path
    if not path.startswith('/v1/') or urllib.parse.urlsplit(path).netloc or '#' in path:
        parser.error('path must be a relative /v1/ API path')
    if args.method != 'GET' and not args.key:
        parser.error('--key is required for mutations; reuse it with identical retries')
    body = None
    if args.json_file:
        body = sys.stdin.buffer.read() if args.json_file == '-' else Path(args.json_file).read_bytes()
        json.loads(body)  # Validate only; keep exact bytes for idempotent retries and integer fidelity.
    headers = {'Authorization': 'Bearer ' + token, 'Accept': 'application/json'}
    if body is not None:
        headers['Content-Type'] = 'application/json'
    if args.key:
        headers['Idempotency-Key'] = args.key
    if args.if_match:
        headers['If-Match'] = args.if_match
    if args.if_none_match:
        headers['If-None-Match'] = '*'
    output = open(args.output, 'xb', opener=lambda p, f: os.open(p, f, 0o600)) if args.output else None
    completed = False
    try:
        # Redirects are rejected: credentials stay scoped to the configured endpoint.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, hdrs, newurl):
                return None
        request = urllib.request.Request(base + path, data=body, headers=headers, method=args.method)
        try:
            response = urllib.request.build_opener(NoRedirect()).open(request, timeout=30)
        except urllib.error.HTTPError as error:
            with error:
                retry = error.headers.get("Retry-After")
                print("HTTP " + str(error.code) + ("; Retry-After: " + retry if retry else ""), file=sys.stderr, flush=True)
                sys.stderr.buffer.write(error.read())
            return 1
        with response:
            result = response.read()
        if output:
            output.write(result)
            output.flush()
            os.fsync(output.fileno())
        else:
            sys.stdout.buffer.write(result)
            sys.stdout.buffer.flush()
        completed = True
        return 0
    finally:
        if output:
            output.close()
            if not completed:
                Path(args.output).unlink(missing_ok=True)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, urllib.error.URLError):
        print('Request or local file operation failed; check connection, input, and any uncertain mutation result.', file=sys.stderr)
        sys.exit(1)
