import json
import time
import urllib.error
import urllib.request
from pathlib import Path


def wait(config_path='/etc/claude-cage/browser-control.json', endpoints=None, timeout=60):
    token = json.loads(Path(config_path).read_text())['token']
    endpoints = endpoints or (
        ('http://127.0.0.1:18080/healthz', False),
        ('http://127.0.0.1:16080/vnc.html', False),
        ('http://127.0.0.1:18500/health', True),
    )
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + timeout
    # The services usually answer within a second or two of starting. Poll
    # quickly at first, so the start does not wait up to a further second
    # after they are ready.
    delay = 0.1
    while time.monotonic() < deadline:
        try:
            for address, browser in endpoints:
                headers = {'Authorization': 'Bearer ' + token} if browser else {}
                request = urllib.request.Request(address, headers=headers)
                with opener.open(request, timeout=2) as response:
                    if browser and json.loads(response.read(1024)).get('ready') is not True:
                        raise ValueError('Browser is starting')
            return
        except (OSError, ValueError, urllib.error.URLError):
            time.sleep(delay)
            delay = min(delay * 2, 1)
    raise SystemExit('Cage browser, editor or display service did not become ready. Inspect the guest service logs and retry.')


if __name__ == '__main__':
    wait()
