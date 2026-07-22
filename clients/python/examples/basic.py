"""End-to-end smoke test against a running harness-chatd.

Usage:
    cd clients/python && PYTHONPATH=. python3 examples/basic.py /usr/local/bin/codex codex
"""

import sys
from harness_chat import Client


def main() -> None:
    if len(sys.argv) != 3:
        print("usage: basic.py <binary_path> <harness>", file=sys.stderr)
        sys.exit(2)
    binary, harness = sys.argv[1], sys.argv[2]

    client = Client("http://127.0.0.1:8080")
    conv = client.open(harness=harness, binary_path=binary)
    print(f"opened conversation {conv.id}")
    try:
        with conv.control():
            turn_id = conv.send("hello")
            print(f"sent turn {turn_id}")
            for ev in conv.events():
                if ev.type != "turn" or ev.turn is None:
                    continue
                print(f"  event: turn={ev.turn.id} state={ev.turn.state}")
                if ev.turn.id == turn_id and ev.turn.state in ("complete", "errored"):
                    break
        for t in conv.history():
            print(f"  {t.role}: {t.text[:80]!r}")
    finally:
        conv.close()


if __name__ == "__main__":
    main()
