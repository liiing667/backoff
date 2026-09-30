# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `TimeBudget` and `WithTimeBudget`: a wall-clock budget shared across nested or sequential `Retry` calls. Retry charges elapsed attempt and wait time against it before each retry and stops with `ErrMaxElapsedTime` instead of sleeping when the remaining budget cannot cover the next backoff wait.
- `ErrorClassifier` and `WithErrorClassifier`: per-error-class retry allowances, counted independently per class. Exhausting an allowance reports `ErrExhausted` like `WithMaxTries`.
- `WithRandomizationFactor`: configures the jitter ratio of the default exponential backoff policy.

## [7.0.0] - 2026-06-30

### Changed

- `RetryAfter` now takes a `time.Duration` and a required cause error: `RetryAfter(d time.Duration, cause error)`. The cause is preserved as `RetryError.LastErr` when retrying stops. (#184)

### Added

- `RetryAfterError.Err`, exposed via `Unwrap`.

## [6.0.0] - 2026-06-16

### Added

- `RetryError`, returned by `Retry` on every failure, exposing the last operation error (`LastErr`) and the reason it stopped (`Cause`). (#181)
- `ErrPermanent`, `ErrExhausted`, and `ErrMaxElapsedTime` cause sentinels, plus `AsRetryError` to extract a `*RetryError` from an error chain.

### Changed

- `Retry` now returns a `*RetryError` on any failure instead of the bare context or operation error. Inspect it with `errors.Is`/`errors.As`; the last operation error is always preserved.
- `WithMaxElapsedTime` now reports `ErrMaxElapsedTime` instead of `context.DeadlineExceeded`.

### Removed

- The `PermanentError` type. Use `Permanent(err)` (unchanged) and detect it with `errors.Is(err, ErrPermanent)`.

See [`ExampleRetry_outcomes`](example_test.go) for how to inspect error outcomes.

## [5.0.0] - 2024-12-19

### Added

- RetryAfterError can be returned from an operation to indicate how long to wait before the next retry.

### Changed

- Retry function now accepts additional options for specifying max number of tries and max elapsed time.
- Retry function now accepts a context.Context.
- Operation function signature changed to return result (any type) and error.

### Removed

- RetryNotify* and RetryWithData functions. Only single Retry function remains.
- Optional arguments from ExponentialBackoff constructor.
- Clock and Timer interfaces.

### Fixed

- The original error is returned from Retry if there's a PermanentError. (#144)
- The Retry function respects the wrapped PermanentError. (#140)
