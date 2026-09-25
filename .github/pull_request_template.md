## Summary

<!-- What changes and why. Link the design document section it implements. -->

## Checklist

- [ ] The title follows Conventional Commits: `<type>(<scope>): <subject>`.
- [ ] Every commit is signed off (`git commit -s`).
- [ ] Tests cover the change; `make hygiene lint generate build test supply-chain` passes locally.
- [ ] Generated files are regenerated and committed (`make generate`).
- [ ] Documentation is updated in `docs/` where behavior or a decision changed.
- [ ] A new dependency has a Tech stack catalog row and a depguard allow list entry.

## References

<!-- Refs: OQ-<docslug>-<n>, ADR-NNNN, issues. -->
