from pydantic import BaseModel, validator


class Config(BaseModel):
    # Field default=0.
    timeout: int = Field(default=0)


class OtherConfig(BaseModel):
    # Validator is in a DIFFERENT class — no join key match.
    # Expected: 0 leads.
    @validator('timeout')
    @classmethod
    def validate_timeout(cls, v):
        if v <= 0:
            raise ValueError('timeout must be positive')
        return v
