# Contributing to wolweb

Thanks for your interest! Bug reports, ideas and pull requests are welcome.

## Reporting issues

Open a GitHub issue with what you expected, what happened, your `wolweb` version (a release tag or commit) and the relevant part of your config (remove anything private).

## Development

Requires Go (see `go.mod`).

```bash
make build   # compile ./wolweb
make test    # go test -race ./...
```

CI also runs [golangci-lint](https://golangci-lint.run) using `.golangci.yml`; please make sure it passes locally if you have it installed.

## Pull requests

1. Fork the repo and create a branch from `main`.
2. Keep changes focused and add or update tests where it makes sense.
3. Update the README and `wolweb.toml.example` if you add or change an option.
4. Make sure `make test` passes.
5. Open a pull request against `main`.

### Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org), for example:

```
feat: show cooldown notice and disable wake button
fix: resolve golangci-lint findings
docs: add screenshots to README
```

Release notes are generated from these messages, so the type matters: `feat` and `fix` appear in the changelog, while `docs`, `chore`, `test` and `ci` are left out.

## Releases

Releases are made by the maintainer from the **Release** workflow in GitHub Actions (see the README). Contributors don't need to tag or version anything: once your pull request is merged, it will be included in the next release.

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
