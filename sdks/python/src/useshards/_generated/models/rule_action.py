from enum import StrEnum


class RuleAction(StrEnum):
    ALLOW = "allow"
    DENY = "deny"

    def __str__(self) -> str:
        return str(self.value)
