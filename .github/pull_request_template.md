## Summary

<!-- Concise explanation of the changes made and the motivation behind them -->

## Type of Change

- [ ] Bug fix (non-breaking change fixing an issue)
- [ ] New feature (non-breaking change adding functionality)
- [ ] Breaking change (fix or feature causing existing functionality to change)
- [ ] Performance improvement / refactor
- [ ] Documentation / tooling enhancement

## Verification & Testing

<!-- Describe the tests executed and attach evidence -->

- [ ] Ran `make check` locally (all unit tests, linting, formatting, and installer checks pass)
- [ ] Ran integration tests (`make test-integration`) if modifying network, server, or streaming logic
- [ ] Ran race detector (`go test -race ./...`) without race conditions
- [ ] Added unit or integration tests covering the new behavior

## Checklist

- [ ] My code adheres to the project's Go style guidelines and modern idioms.
- [ ] All new exported functions, types, and struct fields have clear English doc comments.
- [ ] Any upstream Google Cloud Code Assist quirks or translation subtleties are documented.
- [ ] No credentials, tokens, or personal identifiers are committed or printed in logs.
- [ ] Documentation has been updated to reflect any user-facing CLI or configuration changes.
