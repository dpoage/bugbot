from pydantic import BaseModel, validator


class ServerConfig(BaseModel):
    # Negative: default=5 is not rejected by validator (which rejects <= 0).
    # Expected: 0 leads.
    timeout: int = Field(default=5)

    @validator('timeout')
    @classmethod
    def validate_timeout(cls, v):
        if v <= 0:
            raise ValueError('timeout must be positive')
        return v
