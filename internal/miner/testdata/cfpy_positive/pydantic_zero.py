from pydantic import BaseModel, validator


class ServerConfig(BaseModel):
    # Positive: default=0 but validator rejects v <= 0.
    # Expected: lead at the field declaration line.
    timeout: int = Field(default=0)
    retries: int = Field(default=3)

    @validator('timeout')
    @classmethod
    def validate_timeout(cls, v):
        if v <= 0:
            raise ValueError('timeout must be positive')
        return v
