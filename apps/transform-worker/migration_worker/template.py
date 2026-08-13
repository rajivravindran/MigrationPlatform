"""Pydantic models matching the shared rule-template JSON Schema."""
from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class SourceField(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str
    type: Literal["string", "integer", "number", "boolean", "datetime", "object"]


class Source(BaseModel):
    model_config = ConfigDict(extra="forbid")
    type: Literal["csv", "json", "xml", "salesforce", "watched_prefix"]
    schema_: list[SourceField] | None = Field(default=None, alias="schema")
    options: dict[str, Any] | None = None
    connectorId: int | None = None


class PreprocessStep(BaseModel):
    model_config = ConfigDict(extra="forbid")
    id: str
    field: str
    fn: str
    args: dict[str, Any] | None = None
    code: str | None = None


class Auth(BaseModel):
    type: Literal["bearer", "basic", "api_key", "oauth2_client"]
    secretRef: str | None = None
    headerName: str | None = None


class Destination(BaseModel):
    model_config = ConfigDict(extra="forbid")
    type: Literal["http"]
    method: Literal["GET", "POST", "PUT", "PATCH", "DELETE"]
    # Plain str (not HttpUrl): URLs may contain {placeholder} path templating.
    url: str
    pathParams: dict[str, Any] | None = None
    queryParams: dict[str, Any] | None = None
    headers: dict[str, Any] | None = None
    auth: Auth | None = None
    idempotencyKey: Any | None = None
    idempotencyHeader: str | None = None


class Retry(BaseModel):
    model_config = ConfigDict(extra="forbid")
    maxAttempts: int | None = Field(default=None, ge=1, le=20)
    backoff: Literal["exponential", "fixed"] | None = None
    initialIntervalMs: int | None = Field(default=None, ge=10)


class Mapping(BaseModel):
    model_config = ConfigDict(extra="forbid")
    payload: dict[str, Any]


class Step(BaseModel):
    """One HTTP call in a multi-step chain."""

    model_config = ConfigDict(extra="forbid")
    name: str = Field(pattern=r"^[A-Za-z_][A-Za-z0-9_]*$")
    description: str | None = None
    mapping: Mapping | None = None
    destination: Destination
    onFailure: Literal["stop", "continue"] | None = None


class ExportColumn(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)
    name: str
    from_field: str | None = Field(default=None, alias="$from")
    from_response: str | None = Field(default=None, alias="$fromResponse")
    path: str | None = None


class ExportSpec(BaseModel):
    model_config = ConfigDict(extra="forbid")
    columns: list[ExportColumn] | None = None


class RuleTemplate(BaseModel):
    model_config = ConfigDict(extra="forbid")
    id: str
    version: int = Field(ge=1)
    name: str
    source: Source
    preprocess: list[PreprocessStep]
    # Exactly one of (mapping + destination) or steps -- enforced below.
    mapping: Mapping | None = None
    destination: Destination | None = None
    steps: list[Step] | None = Field(default=None, min_length=1, max_length=10)
    retry: Retry | None = None
    concurrency: int | None = Field(default=None, ge=1, le=512)
    export: ExportSpec | None = None

    @model_validator(mode="after")
    def _one_shape(self) -> "RuleTemplate":
        has_single = self.mapping is not None and self.destination is not None
        has_steps = self.steps is not None
        if has_single == has_steps:
            raise ValueError(
                "template must have either mapping+destination or steps, not both/neither"
            )
        return self

    def execution_steps(self) -> list[Step]:
        """Normalize either template shape into an ordered step list."""
        if self.steps is not None:
            return self.steps
        assert self.mapping is not None and self.destination is not None
        return [Step(name="main", mapping=self.mapping, destination=self.destination)]
