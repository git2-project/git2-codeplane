## What and why

<!-- One or two sentences. Link the issue or the version note if there is one. -->

## Checklist

- [ ] Tests added or updated, and `make tests` and `make lint` pass
- [ ] Can anyone outside this repo notice this change (proto, event, config, error, limit, behaviour)?
  If yes: a version note is added under `docs/version_notes/`, and its line is at the top of `docs/version_notes/overview.md`
- [ ] Contract change (`proto/git/v1/`)? `make proto` was run, and every affected owner approved
- [ ] Needs a table or column? Migration PR on git2: <!-- link --> (the copy in `migrations/` matches it)
- [ ] No secrets, tokens, DSNs or `.env` in the diff
