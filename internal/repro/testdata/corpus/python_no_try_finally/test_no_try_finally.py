import unittest


class TestNoTryFinally(unittest.TestCase):
    """Non-behavioral: asserts setLoading.ts's SOURCE TEXT has no
    try/finally around its body. Static check on file contents — ran in
    ~0.000s, never executed setLoading() at all.

    Mirrors the_cloud finding 0000019f396c9337e7cbf18156f91069 ("setLoading
    no try/finally"), one of the 4 false-T1 promotions in bugbot-qb4r.
    """

    def test_missing_try_finally(self):
        with open("src/state/setLoading.ts") as f:
            src = f.read()
        self.assertNotIn("finally", src)


if __name__ == "__main__":
    unittest.main()
