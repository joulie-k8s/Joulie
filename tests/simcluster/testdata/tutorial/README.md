# A tutorial for TestTU01

The runner runs this block:

```bash
echo first >>"$TU_OUT"
```

A text block is output, not a command:

```text
echo never
```

<!-- tutorials: skip -->
```bash
echo skipped >>"$TU_OUT"
```

<!-- tutorials: check
grep first "$TU_OUT" >/dev/null
echo checked >>"$TU_OUT"
-->

```bash
echo last >>"$TU_OUT"
```

<!-- tutorials: check
[ "${TU_FAIL:-}" != 1 ]
-->
