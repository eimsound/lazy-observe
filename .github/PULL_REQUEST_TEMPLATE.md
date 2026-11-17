# Pull request

## Summary

Describe the user-visible outcome and the implementation scope.

## Validation

List every command run and its real result. Do not mark a check complete if it was not run.

- [ ] Relevant unit tests pass (`mise run test`).
- [ ] Go vet passes (`mise run vet`) when Go code changed.
- [ ] Lint passes (`mise run lint`) when Go code changed.
- [ ] Web typecheck/build passes (`mise run web`) when web code changed.
- [ ] Real storage E2E passes (`mise run e2e`) when gateway, isolation, or storage behavior changed.
- [ ] HA E2E passes (`mise run e2e-ha`) when clustering, shared state, S3, probes, or shutdown changed.
- [ ] Relevant performance gates pass (`mise run perf-release`, `mise run perf-ha`, or `mise run performance-gates`) when packaging or performance-sensitive behavior changed; `e2e-ha` alone is not a performance gate.
- [ ] PostgreSQL dialect tests pass (`mise run test-meta-pg`) when metadata schema/query behavior changed.
- [ ] Client SDK checks pass (`mise run sdk-go`, `sdk-node`, `sdk-browser`, or `sdk-python`) when `sdk/` changed; protocol changes update `docs/version-matrix.md` in this PR.
- [ ] Workflows pass static validation (`mise run workflow-check`, plus `mise run ci-changes-test` when path grouping changed) when `.github/` changed, and the pull request's `ci-ok` check passes; report both results separately.

## Change checks

- [ ] Tests cover the changed behavior.
- [ ] Public documentation and examples match the implementation.
- [ ] `CHANGELOG.md` is updated when delivered behavior changed.
- [ ] `config.example.yaml` is updated for configuration changes.
- [ ] SQLite and PostgreSQL migrations share the same new version and logical change.
- [ ] New dependencies are Apache-2.0, MIT, or BSD licensed; no AGPL/GPL dependency was added.
- [ ] `CGO_ENABLED=0` and supported cross-builds remain viable.
- [ ] Project isolation, token handling, and replay privacy were considered.

## Notes for reviewers

Call out migrations, compatibility breaks, follow-up work, or checks that could not be run and why.
