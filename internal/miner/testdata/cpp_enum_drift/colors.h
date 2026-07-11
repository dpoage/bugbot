/* Fixture: enum declaration with explicit values.
   This file has HasError=true due to grammar limitation — enumerator
   extraction still works via the enumerator query. */
typedef enum {
    COLOR_RED   = 0,
    COLOR_GREEN = 1,
    COLOR_BLUE  = 2
} Color;
