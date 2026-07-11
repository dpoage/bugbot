from pydantic import BaseModel, validator


class Config(BaseModel):
    """
    Configuration options.

    timeout: int = Field(default=0)
        Set to 0 to disable. The validator below enforces positive values.
    """
    # Only real_field is a proper field — the docstring line is NOT an assignment.
    real_field: int = Field(default=5)

    @validator('timeout')
    @classmethod
    def validate_timeout(cls, v):
        if v <= 0:
            raise ValueError('timeout must be positive')
        return v
