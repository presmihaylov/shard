from enum import StrEnum


class ExecState(StrEnum):
    EXITED = "exited"
    RUNNING = "running"

    def __str__(self) -> str:
        return str(self.value)
