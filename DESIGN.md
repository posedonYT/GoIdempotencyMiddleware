# Design

Record architectural decisions here as you implement each stage of `idempotency-plan.md`.

`recorder` trusts its caller for `limit` (including 0, which overflows on any non-empty body); `WithMaxBodySize` validates the value.
