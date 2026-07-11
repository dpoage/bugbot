from pydantic import BaseModel, validator


class Outer(BaseModel):
    # Outer class has default=0 for timeout.
    timeout: int = Field(default=0)

    class Inner(BaseModel):
        # Inner class has its own port field.
        port: int = Field(default=0)

        # This validator is for Inner.timeout (referring to a field in Inner,
        # not Outer). The join key is (Inner classStart, 'timeout'), which does
        # NOT match (Outer classStart, 'timeout'). Expected: 0 leads.
        @validator('timeout')
        @classmethod
        def validate_timeout(cls, v):
            if v <= 0:
                raise ValueError('timeout must be positive')
            return v
