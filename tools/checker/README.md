# Go struct layout checker

Run from the repository root:

```sh
go run ./tools/checker .
go test -count=1 ./tools/checker
```

This standard-library-only command uses Go AST and type information to check struct declarations and nonempty struct literals, including generic, nested and inferred elements. It applies to production code and tests selected by the current Go build configuration. Empty structs and non-struct map/slice literals are not treated as violations.

It reports layout violations with file/line locations and exits nonzero. Dependency/type-loading failures also fail the check rather than reporting a false zero. It does not rewrite source code. Run `gofmt -l .` separately; gofmt alone does not enforce one struct field per line.

Other platforms/build tags must be checked under their corresponding Go build configuration. This check establishes layout compliance, not protocol or runtime correctness.
