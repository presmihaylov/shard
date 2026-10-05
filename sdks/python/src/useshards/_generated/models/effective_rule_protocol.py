from enum import StrEnum


class EffectiveRuleProtocol(StrEnum):
    TCP = "tcp"
    UDP = "udp"

    def __str__(self) -> str:
        return str(self.value)
