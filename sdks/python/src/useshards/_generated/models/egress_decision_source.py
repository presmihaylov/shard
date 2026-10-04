from enum import StrEnum


class EgressDecisionSource(StrEnum):
    DNS = "dns"
    HOST = "host"
    PROXY = "proxy"

    def __str__(self) -> str:
        return str(self.value)
