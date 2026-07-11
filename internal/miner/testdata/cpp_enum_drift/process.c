/* Fixture: switch over Color enum with defects.
   Type-A: case 1 uses raw integer equal to COLOR_GREEN=1 (use the name).
   Type-B: COLOR_BLUE not handled (no default). */

void process(Color c) {
    switch (c) {
        case COLOR_RED:
            break;
        case 1:            /* bug: raw integer == COLOR_GREEN; use COLOR_GREEN */
            break;
        /* COLOR_BLUE not handled — type-B lead */
    }
}
