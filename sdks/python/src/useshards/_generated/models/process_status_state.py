from enum import StrEnum


class ProcessStatusState(StrEnum):
    EXITED = "exited"
    GAVE_UP = "gave-up"
    KILLED = "killed"
    RESTARTING = "restarting"
    RUNNING = "running"
    STOPPED = "stopped"

    def __str__(self) -> str:
        return str(self.value)
