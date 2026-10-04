from enum import StrEnum


class DestinationKind(StrEnum):
    CIDR = "cidr"
    DOMAIN = "domain"
    DOMAIN_SUFFIX = "domain-suffix"
    GROUP = "group"

    def __str__(self) -> str:
        return str(self.value)
