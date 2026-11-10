#!/usr/bin/env python3
"""Control client for cmd/mock-discord, used by test/live/claude_smoke.sh.

usage: mockctl.py PORT COMMAND [ARGS]

  message CHANNEL CONTENT     inject a MESSAGE_CREATE from the allowed user
  click CHANNEL CUSTOM_ID     inject a button click from the allowed user
  thread                      print the id of the first created thread (exit 1 if none)
  threads                     print how many threads were created
  component CONTENT [INDEX]   print custom_id of INDEX-th button of the first
                              posted message containing CONTENT (exit 1 if none)
  components CONTENT          print how many posted messages containing CONTENT carry buttons
  count CHANNEL SUBSTR        print how many messages/edits to CHANNEL contain SUBSTR
  texts CHANNEL               print the latest content of every message in CHANNEL
"""
import json
import sys
import urllib.request

USER = "live-user"


def call(port, path, data=None):
    url = "http://127.0.0.1:%s%s" % (port, path)
    req = urllib.request.Request(url, data=None if data is None else json.dumps(data).encode(),
                                 headers={"Content-Type": "application/json"},
                                 method="GET" if data is None else "POST")
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read()
        return json.loads(body) if body else None


def calls(port):
    return call(port, "/control/calls") or []


def body_of(c):
    b = c.get("body")
    return b if isinstance(b, dict) else {}


def latest_texts(port, channel):
    """Latest content per message id: a PATCH replaces the text of its message."""
    texts = {}
    order = []
    for c in calls(port):
        if c["channel_id"] != channel:
            continue
        content = body_of(c).get("content")
        if content is None:
            continue
        if c["method"] == "POST" and c["path"].endswith("/messages"):
            texts[c["id"]] = content
            order.append(c["id"])
        elif c["method"] in ("PATCH", "PUT") and "/messages/" in c["path"]:
            mid = c["path"].rsplit("/", 1)[-1]
            if mid in texts:
                texts[mid] = content
    return [texts[i] for i in order]


def main():
    port, cmd, args = sys.argv[1], sys.argv[2], sys.argv[3:]
    if cmd == "message":
        call(port, "/control/dispatch", {"t": "MESSAGE_CREATE", "d": {
            "id": "live-msg-%d" % len(calls(port)), "channel_id": args[0], "content": args[1],
            "author": {"id": USER, "bot": False}, "attachments": []}})
    elif cmd == "click":
        call(port, "/control/dispatch", {"t": "INTERACTION_CREATE", "d": {
            "id": "live-int-%d" % len(calls(port)), "application_id": "app", "type": 3, "token": "token",
            "version": 1, "channel_id": args[0],
            "member": {"user": {"id": USER, "bot": False}},
            "data": {"custom_id": args[1], "component_type": 2}}})
    elif cmd == "thread":
        for c in calls(port):
            if c["method"] == "POST" and c["path"].endswith("/threads") and body_of(c).get("type") == 11:
                print(c["id"])
                return
        sys.exit(1)
    elif cmd == "threads":
        print(sum(1 for c in calls(port) if c["method"] == "POST" and c["path"].endswith("/threads")))
    elif cmd == "component":
        idx = int(args[1]) if len(args) > 1 else 0
        for c in calls(port):
            b = body_of(c)
            comps = b.get("components") or []
            if args[0] in (b.get("content") or "") and comps and len(comps[0].get("components", [])) > idx:
                print(comps[0]["components"][idx]["custom_id"])
                return
        sys.exit(1)
    elif cmd == "components":
        print(sum(1 for c in calls(port) if c["method"] == "POST" and body_of(c).get("components")
                  and args[0] in (body_of(c).get("content") or "")))
    elif cmd == "count":
        print(sum(1 for t in latest_texts(port, args[0]) if args[1] in t))
    elif cmd == "texts":
        for t in latest_texts(port, args[0]):
            print(json.dumps(t))
    else:
        sys.exit("unknown command %s" % cmd)


main()
