from enum import StrEnum


class RuleProtocol(StrEnum):
    TCP = "tcp"
    UDP = "udp"

    def __str__(self) -> str:
        return str(self.value)
