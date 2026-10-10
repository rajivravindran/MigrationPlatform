import os
import time
from collections.abc import Callable


def child_pids() -> list[int]:
    """PIDs (including zombies) whose parent is this test process."""
    me = os.getpid()
    pids = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open(f"/proc/{entry}/stat") as fh:
                stat = fh.read()
        except OSError:
            continue
        # Fields after the parenthesised command name: state, ppid, ...
        fields = stat.rsplit(")", 1)[1].split()
        if int(fields[1]) == me:
            pids.append(int(entry))
    return pids


def wait_for(predicate: Callable[[], bool], timeout_s: float = 5.0) -> None:
    deadline = time.monotonic() + timeout_s
    while not predicate():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        time.sleep(0.01)
