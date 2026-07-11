/* dir_b: Color enum with DIFFERENT members — same type name, different set.
   D2 oracle: these must NOT be merged with dir_a's Color. */
typedef enum {
    COLOR_ALPHA = 10,
    COLOR_BETA  = 20,
    COLOR_GAMMA = 30
} Color;
