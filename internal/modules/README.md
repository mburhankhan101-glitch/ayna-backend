# Modules

One directory per bounded context from `03-DDD-and-Onion-Architecture`:

    iam/  skinanalysis/  skinprofile/  recommendations/  growth/  notifications/  billing/

Each follows the same internal layering:

    <module>/
      domain/          entities, value objects, ports (interfaces). Stdlib only.
      application/     use cases. Orchestrates domain, calls ports, never concrete infra.
      infrastructure/  http handlers, postgres repos, storage, queue, vendor clients.

Two rules are enforced by `internal/arch` and fail the build in CI:

1. `domain/` imports **nothing** outside the standard library.
2. No module imports another module's `infrastructure/`.

Cross-module communication goes through the event bus (ADR-001) using the
event names in `02-Domain-Discovery-and-Event-Storming`.
