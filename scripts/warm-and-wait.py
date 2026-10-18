#!/usr/bin/env python3
"""Submit a warming job, print progress, and exit nonzero unless it is Ready."""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', default=os.getenv('KUBEWARM_URL', 'http://127.0.0.1:8080'))
    parser.add_argument('--request', required=True, help='JSON request file')
    args = parser.parse_args()
    token = os.environ.get('KUBEWARM_TOKEN')
    if not token:
        parser.error('set KUBEWARM_TOKEN to the CI token')
    with open(args.request, encoding='utf-8') as stream:
        payload = json.load(stream)
    base = args.url.rstrip('/')
    def call(path, data=None):
        request = urllib.request.Request(base + path, data=data, headers={
            'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    job = call('/v1/warmings', json.dumps(payload).encode())
    deadline = time.monotonic() + payload['timeoutSeconds'] + 60
    print('Job:', job['id'], flush=True)
    while time.monotonic() < deadline:
        job = call('/v1/warmings/' + job['id'])
        print(json.dumps(job), flush=True)
        if job['state'] == 'Ready':
            return 0
        if job['state'] in ('Failed', 'TimedOut'):
            return 1
        time.sleep(5)
    print('Client deadline exceeded', file=sys.stderr)
    return 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (urllib.error.URLError, ValueError, OSError, KeyError) as error:
        print('Warming failed:', error, file=sys.stderr)
        sys.exit(1)
